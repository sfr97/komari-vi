//! UDP relay entrance.

mod socket;
mod sockmap;
mod middle;
mod batched;

use std::io::Result;
use std::net::SocketAddr;
use std::sync::Arc;

use crate::endpoint::Endpoint;

use sockmap::SockMap;
use middle::associate_and_relay;
use tokio::sync::oneshot;

pub trait UdpObserver: Send + Sync + 'static {
    fn on_session_open(&self, peer: SocketAddr);
    fn on_session_close(&self, peer: SocketAddr);
    fn on_bytes(&self, inbound_delta: u64, outbound_delta: u64);
}

/// Launch a udp relay.
pub async fn run_udp(endpoint: Endpoint) -> Result<()> {
    run_udp_inner(endpoint, None, None).await
}

pub async fn run_udp_with_ready(endpoint: Endpoint, ready: oneshot::Sender<Result<()>>) -> Result<()> {
    run_udp_inner(endpoint, Some(ready), None).await
}

pub async fn run_udp_with_ready_and_observer(
    endpoint: Endpoint,
    ready: oneshot::Sender<Result<()>>,
    observer: Arc<dyn UdpObserver>,
) -> Result<()> {
    run_udp_inner(endpoint, Some(ready), Some(observer)).await
}

async fn run_udp_inner(
    endpoint: Endpoint,
    ready: Option<oneshot::Sender<Result<()>>>,
    observer: Option<Arc<dyn UdpObserver>>,
) -> Result<()> {
    let Endpoint {
        laddr,
        raddr,
        bind_opts,
        conn_opts,
        ..
    } = endpoint;

    let sockmap = Arc::new(SockMap::new());
    let run_guard = Arc::new(());
    let run_guard_weak = Arc::downgrade(&run_guard);

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

    let lis = Arc::new(lis);
    let raddr = Arc::new(raddr);
    let conn_opts = Arc::new(conn_opts);
    loop {
        if let Err(e) = associate_and_relay(
            lis.clone(),
            raddr.clone(),
            conn_opts.clone(),
            sockmap.clone(),
            observer.clone(),
            run_guard_weak.clone(),
        )
        .await
        {
            log::error!("[udp]error: {}", e);
        }
    }
}
