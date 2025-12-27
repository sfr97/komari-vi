//! TCP relay entrance.

mod socket;
mod middle;
mod plain;
mod stats;

#[cfg(feature = "balance")]
pub mod health;

#[cfg(feature = "hook")]
mod hook;

#[cfg(feature = "proxy")]
mod proxy;

#[cfg(feature = "transport")]
mod transport;

use std::io::{ErrorKind, Result};
use std::net::SocketAddr;
use std::sync::Arc;

use crate::endpoint::Endpoint;

use middle::connect_and_relay;
use tokio::sync::oneshot;

pub trait TcpObserver: Send + Sync + 'static {
    fn on_connection_open(&self, peer: SocketAddr) -> u64;
    fn on_connection_backend(&self, _id: u64, _backend: &crate::endpoint::RemoteAddr) {}
    fn on_connection_bytes(&self, id: u64, inbound_delta: u64, outbound_delta: u64);
    fn on_connection_end(&self, id: u64, error: Option<String>);

    #[cfg(feature = "balance")]
    fn on_failover_health(&self, _health: Option<std::sync::Arc<health::FailoverHealth>>) {}
}

/// Launch a tcp relay.
pub async fn run_tcp(endpoint: Endpoint) -> Result<()> {
    run_tcp_inner(endpoint, None, None).await
}

pub async fn run_tcp_with_ready(endpoint: Endpoint, ready: oneshot::Sender<Result<()>>) -> Result<()> {
    run_tcp_inner(endpoint, Some(ready), None).await
}

pub async fn run_tcp_with_ready_and_observer(
    endpoint: Endpoint,
    ready: oneshot::Sender<Result<()>>,
    observer: Arc<dyn TcpObserver>,
) -> Result<()> {
    run_tcp_inner(endpoint, Some(ready), Some(observer)).await
}

async fn run_tcp_inner(
    endpoint: Endpoint,
    ready: Option<oneshot::Sender<Result<()>>>,
    observer: Option<Arc<dyn TcpObserver>>,
) -> Result<()> {
    let Endpoint {
        laddr,
        raddr,
        bind_opts,
        conn_opts,
        extra_raddrs,
    } = endpoint;

    #[cfg(feature = "balance")]
    let mut _probe_stop_tx: Option<tokio::sync::oneshot::Sender<()>> = None;

    #[cfg(feature = "balance")]
    let failover_health = {
        use realm_lb::Strategy;
        if conn_opts.balancer.strategy() == Strategy::Failover {
            Some(Arc::new(health::FailoverHealth::new(
                1 + extra_raddrs.len(),
                conn_opts.failover.ok_ttl_ms,
                conn_opts.failover.backoff_base_ms,
                conn_opts.failover.backoff_max_ms,
            )))
        } else {
            None
        }
    };

    #[cfg(feature = "balance")]
    if let Some(obs) = observer.as_ref() {
        obs.on_failover_health(failover_health.clone());
    }

    #[cfg(feature = "balance")]
    if let Some(h) = failover_health.clone() {
        use realm_lb::Strategy;
        let fo = conn_opts.failover;
        if conn_opts.balancer.strategy() == Strategy::Failover && fo.probe_interval_ms > 0 {
            let (stop_tx, probe_stop_rx) = tokio::sync::oneshot::channel::<()>();
            _probe_stop_tx = Some(stop_tx);
            let peers: Vec<(u8, crate::endpoint::RemoteAddr)> = {
                let mut v = Vec::with_capacity(1 + extra_raddrs.len());
                v.push((0, raddr.clone()));
                for (i, addr) in extra_raddrs.iter().cloned().enumerate() {
                    v.push(((i + 1) as u8, addr));
                }
                v
            };
            let probe_opts = conn_opts.clone();
            let mut probe_stop_rx = probe_stop_rx;
            tokio::spawn(async move {
                use futures::stream::{self, StreamExt};
                use tokio::time::{interval, timeout};
                use std::time::Duration;

                async fn probe_one(
                    idx: u8,
                    addr: &crate::endpoint::RemoteAddr,
                    opts: &crate::endpoint::ConnectOpts,
                    h: &health::FailoverHealth,
                    probe_timeout_ms: u64,
                ) {
                    let fut = socket::connect(addr, opts);
                    match timeout(Duration::from_millis(probe_timeout_ms), fut).await {
                        Ok(Ok(_)) => h.mark_ok(idx),
                        Ok(Err(_)) | Err(_) => h.mark_fail(idx),
                    }
                }

                async fn probe_round(
                    peers: &[(u8, crate::endpoint::RemoteAddr)],
                    opts: &crate::endpoint::ConnectOpts,
                    h: &health::FailoverHealth,
                    probe_timeout_ms: u64,
                ) {
                    let concurrency = peers.len().clamp(1, 8);
                    stream::iter(peers.iter())
                        .for_each_concurrent(concurrency, |(idx, addr)| async move {
                            probe_one(*idx, addr, opts, h, probe_timeout_ms).await;
                        })
                        .await;
                }

                // initial warm-up
                probe_round(&peers, &probe_opts, &h, fo.probe_timeout_ms).await;

                let mut itv = interval(Duration::from_millis(fo.probe_interval_ms));
                loop {
                    tokio::select! {
                        _ = itv.tick() => {
                            probe_round(&peers, &probe_opts, &h, fo.probe_timeout_ms).await;
                        }
                        _ = &mut probe_stop_rx => {
                            break;
                        }
                    }
                }
            });
        }
    }

    let raddr = Arc::new(raddr);
    let conn_opts = Arc::new(conn_opts);
    let extra_raddrs = Arc::new(extra_raddrs);

    let lis = match socket::bind(&laddr, bind_opts) {
        Ok(lis) => {
            if let Some(ready) = ready {
                let _ = ready.send(Ok(()));
            }
            lis
        }
        Err(e) => {
            if let Some(ready) = ready {
                let _ = ready.send(Err(std::io::Error::new(e.kind(), e.to_string())));
            }
            return Err(e);
        }
    };
    let keepalive = socket::keepalive::build(conn_opts.as_ref());

    loop {
        let (local, addr) = match lis.accept().await {
            Ok(x) => x,
            Err(e) if e.kind() == ErrorKind::ConnectionAborted => {
                log::warn!("[tcp]failed to accept: {}", e);
                continue;
            }
            Err(e) => {
                log::error!("[tcp]failed to accept: {}", e);
                return Err(e);
            }
        };

        let obs = observer.clone();
        let conn_id = obs.as_ref().map(|o| o.on_connection_open(addr)).unwrap_or_default();
        #[cfg(feature = "balance")]
        let failover_health = failover_health.clone();

        let raddr = raddr.clone();
        let conn_opts = conn_opts.clone();
        let extra_raddrs = extra_raddrs.clone();

        // ignore error
        let _ = local.set_nodelay(true);
        // set tcp_keepalive
        if let Some(kpa) = &keepalive {
            use socket::keepalive::SockRef;
            SockRef::from(&local).set_tcp_keepalive(kpa)?;
        }

        tokio::spawn(async move {
            let res = match obs.clone() {
                Some(obs) => {
                    connect_and_relay(
                        local,
                        raddr.clone(),
                        conn_opts.clone(),
                        extra_raddrs.clone(),
                        #[cfg(feature = "balance")]
                        failover_health,
                        Some((obs, conn_id)),
                    )
                    .await
                }
                None => {
                    connect_and_relay(
                        local,
                        raddr.clone(),
                        conn_opts.clone(),
                        extra_raddrs.clone(),
                        #[cfg(feature = "balance")]
                        failover_health,
                        None,
                    )
                    .await
                }
            };
            match res {
                Ok(()) => {
                    if let Some(obs) = &obs {
                        obs.on_connection_end(conn_id, None);
                    }
                    log::debug!("[tcp]{} => {}, finish", addr, raddr.as_ref());
                }
                Err(e) => {
                    if let Some(obs) = &obs {
                        obs.on_connection_end(conn_id, Some(e.to_string()));
                    }
                    log::error!("[tcp]{} => {}, error: {}", addr, raddr.as_ref(), e);
                }
            };
        });
    }
}
