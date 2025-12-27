use std::io::Result;
use std::time::Duration;
use std::sync::Arc;
use tokio::net::TcpStream;

#[cfg(feature = "balance")]
use std::time::Instant;
use std::future::Future;

use super::socket;
use super::plain;
use super::stats::{CountDirection, CountStream};
use super::TcpObserver;

#[cfg(feature = "hook")]
use super::hook;

#[cfg(feature = "proxy")]
use super::proxy;

#[cfg(feature = "transport")]
use super::transport;

use crate::endpoint::{RemoteAddr, ConnectOpts};

#[cfg(feature = "balance")]
use super::health::FailoverHealth;
#[allow(unused)]
pub async fn connect_and_relay(
    mut local: TcpStream,
    raddr: Arc<RemoteAddr>,
    conn_opts: Arc<ConnectOpts>,
    extra_raddrs: Arc<Vec<RemoteAddr>>,
    #[cfg(feature = "balance")] failover_health: Option<std::sync::Arc<FailoverHealth>>,
    observer: Option<(std::sync::Arc<dyn TcpObserver>, u64)>,
) -> Result<()> {
    let local_peer = local.peer_addr()?;

    async fn local_is_closed(local: &TcpStream) -> bool {
        let mut b = [0u8; 1];
        match local.peek(&mut b).await {
            Ok(0) => true,
            Ok(_) => false,
            Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => false,
            Err(_) => true,
        }
    }

    async fn connect_with_local_cancel<F>(local: &TcpStream, fut: F) -> Result<tokio::net::TcpStream>
    where
        F: Future<Output = Result<tokio::net::TcpStream>>,
    {
        tokio::pin!(fut);
        loop {
            tokio::select! {
                res = &mut fut => return res,
                _ = tokio::time::sleep(Duration::from_millis(100)) => {
                    if local_is_closed(local).await {
                        return Err(std::io::Error::new(std::io::ErrorKind::BrokenPipe, "client disconnected"));
                    }
                }
            }
        }
    }

    let ConnectOpts {
        #[cfg(feature = "proxy")]
        proxy_opts,

        #[cfg(feature = "transport")]
        transport,

        #[cfg(feature = "balance")]
        balancer,

        #[cfg(feature = "balance")]
        failover,

        tcp_keepalive,
        ..
    } = conn_opts.as_ref();

    // before connect:
    // - pre-connect hook
    // - load balance
    // ..
    let raddr0 = raddr.as_ref();
    let extras = extra_raddrs.as_ref();

    #[cfg(feature = "hook")]
    let hook_selected: Option<&RemoteAddr> = {
        // accept or deny connection.
        #[cfg(feature = "balance")]
        {
            hook::pre_connect_hook(&mut local, raddr0, extras).await?;
            None
        }

        // accept or deny connection, or select a remote peer.
        #[cfg(not(feature = "balance"))]
        {
            Some(hook::pre_connect_hook(&mut local, raddr0, extras).await?)
        }
    };

    #[cfg(not(feature = "hook"))]
    let hook_selected: Option<&RemoteAddr> = None;

    #[cfg(feature = "balance")]
    let (is_failover, balance_candidates): (bool, Vec<(u8, &RemoteAddr)>) = {
        use realm_lb::{BalanceCtx, Strategy, Token};
        let src_ip = local_peer.ip();
        let tokens = balancer.candidates(BalanceCtx { src_ip: &src_ip });
        log::debug!("[tcp]candidate remote peers: {:?}", tokens);

        let is_failover = balancer.strategy() == Strategy::Failover;
        let mut out = Vec::with_capacity(tokens.len().max(1));
        for token in tokens {
            match token {
                Token(0) => out.push((0, raddr0)),
                Token(idx) => match extras.get(idx.saturating_sub(1) as usize) {
                    Some(x) => out.push((idx, x)),
                    None => log::warn!("[tcp]invalid remote peer token: {:?}", token),
                },
            }
        }
        if out.is_empty() {
            out.push((0, raddr0));
        }
        (is_failover, out)
    };

    #[cfg(not(feature = "balance"))]
    let balance_candidates: Vec<(u8, &RemoteAddr)> = vec![(0, raddr0)];

    #[cfg(feature = "balance")]
    let candidates: Vec<(u8, &RemoteAddr)> = match hook_selected {
        Some(x) => vec![(0, x)],
        None => balance_candidates,
    };

    #[cfg(not(feature = "balance"))]
    let candidates: Vec<(u8, &RemoteAddr)> = match hook_selected {
        Some(x) => vec![(0, x)],
        None => balance_candidates,
    };

    // connect! (failover strategy: prefer recent healthy, otherwise skip down and fail-fast)
    let mut last_err: Option<std::io::Error> = None;
    let mut selected_raddr: Option<&RemoteAddr> = None;
    let mut remote: Option<tokio::net::TcpStream> = None;

    #[cfg(feature = "balance")]
    let failover_health = if is_failover { failover_health } else { None };

    #[cfg(feature = "balance")]
    let retry_window_ms = if is_failover { failover.retry_window_ms } else { 0 };
    #[cfg(feature = "balance")]
    let retry_sleep_ms = if is_failover { failover.retry_sleep_ms } else { 0 };
    #[cfg(feature = "balance")]
    let start = Instant::now();

    loop {
        if local_is_closed(&local).await {
            return Err(std::io::Error::new(
                std::io::ErrorKind::BrokenPipe,
                "client disconnected",
            ));
        }

        #[cfg(feature = "balance")]
        let allowed: Vec<(u8, &RemoteAddr)> = if let Some(h) = &failover_health {
            let mut out: Vec<(u8, &RemoteAddr)> = candidates
                .iter()
                .copied()
                .filter(|(idx, _)| !h.should_skip(*idx))
                .collect();
            if out.is_empty() {
                out = candidates.clone();
            }
            out
        } else {
            candidates.clone()
        };

        #[cfg(not(feature = "balance"))]
        let allowed: Vec<(u8, &RemoteAddr)> = candidates.clone();

        for (idx, candidate) in allowed {
            #[cfg(feature = "balance")]
            let use_failfast = failover_health.as_ref().map(|h| !h.is_recent_ok(idx)).unwrap_or(false);

            #[cfg(feature = "balance")]
            let connect_res = if use_failfast && is_failover && failover.failfast_timeout_ms > 0 {
                connect_with_local_cancel(&local, async {
                    match tokio::time::timeout(
                        Duration::from_millis(failover.failfast_timeout_ms),
                        socket::connect(candidate, conn_opts.as_ref()),
                    )
                    .await
                    {
                        Ok(r) => r,
                        Err(_) => Err(std::io::Error::new(
                            std::io::ErrorKind::TimedOut,
                            "connect failfast timeout",
                        )),
                    }
                })
                .await
            } else {
                connect_with_local_cancel(&local, socket::connect(candidate, conn_opts.as_ref())).await
            };

            #[cfg(not(feature = "balance"))]
            let connect_res = connect_with_local_cancel(&local, socket::connect(candidate, conn_opts.as_ref())).await;

            match connect_res {
                Ok(stream) => {
                    selected_raddr = Some(candidate);
                    remote = Some(stream);
                    #[cfg(feature = "balance")]
                    if let Some(h) = &failover_health {
                        h.mark_ok(idx);
                    }
                    break;
                }
                Err(e) => {
                    last_err = Some(e);
                    #[cfg(feature = "balance")]
                    if let Some(h) = &failover_health {
                        h.mark_fail(idx);
                    }
                }
            }
        }

        if remote.is_some() {
            break;
        }

        #[cfg(feature = "balance")]
        {
            if retry_window_ms == 0 {
                break;
            }
            let elapsed_ms = start.elapsed().as_millis() as u64;
            if elapsed_ms >= retry_window_ms {
                break;
            }
            if retry_sleep_ms > 0 {
                tokio::time::sleep(Duration::from_millis(retry_sleep_ms)).await;
            } else {
                tokio::task::yield_now().await;
            }
        }

        #[cfg(not(feature = "balance"))]
        break;
    }

    let selected_raddr = selected_raddr.unwrap_or(raddr0);
    let mut remote = match remote {
        Some(x) => x,
        None => {
            return Err(last_err.unwrap_or_else(|| {
                std::io::Error::new(std::io::ErrorKind::InvalidInput, "could not connect to any remote peer")
            }))
        }
    };

    if let Some((obs, id)) = observer.as_ref() {
        obs.on_connection_backend(*id, selected_raddr);
    }

    log::info!("[tcp]{} => {} as {}", local_peer, selected_raddr, remote.peer_addr()?);

    // after connected
    // ..
    #[cfg(feature = "proxy")]
    if proxy_opts.enabled() {
        proxy::handle_proxy(&mut local, &mut remote, *proxy_opts).await?;
    }

    let res: Result<()> = if let Some((obs, id)) = observer {
        let local = CountStream::new(local, obs.clone(), id, CountDirection::Inbound);
        let remote = CountStream::new(remote, obs, id, CountDirection::Outbound);

        #[cfg(feature = "transport")]
        {
            if let Some((ac, cc)) = transport {
                transport::run_relay(local, remote, ac, cc).await
            } else {
                plain::run_relay(local, remote).await
            }
        }
        #[cfg(not(feature = "transport"))]
        {
            plain::run_relay(local, remote).await
        }
    } else {
        #[cfg(feature = "transport")]
        {
            if let Some((ac, cc)) = transport {
                transport::run_relay(local, remote, ac, cc).await
            } else {
                plain::run_relay(local, remote).await
            }
        }
        #[cfg(not(feature = "transport"))]
        {
            plain::run_relay(local, remote).await
        }
    };

    // ignore relay error
    match res {
        Ok(()) => Ok(()),
        Err(e) => {
            log::debug!("[tcp]forward error: {}, ignored", e);
            Ok(())
        }
    }
}
