//! Relay endpoint.

use std::fmt::{Display, Formatter};
use std::net::SocketAddr;

#[cfg(feature = "transport")]
use kaminari::mix::{MixAccept, MixConnect};

#[cfg(feature = "balance")]
use realm_lb::Balancer;

/// Failover-specific options.
///
/// All durations are milliseconds.
#[cfg(feature = "balance")]
#[derive(Debug, Clone, Copy)]
pub struct FailoverOpts {
    /// Enable background probing when > 0.
    pub probe_interval_ms: u64,
    /// Per-probe connect timeout.
    pub probe_timeout_ms: u64,
    /// Fail-fast timeout used when a peer is not recently healthy.
    pub failfast_timeout_ms: u64,
    /// Consider a peer "healthy" if it had a successful connect within this TTL.
    pub ok_ttl_ms: u64,
    /// Base backoff after failures.
    pub backoff_base_ms: u64,
    /// Max backoff after failures.
    pub backoff_max_ms: u64,
    /// When > 0, retry connect attempts within this window before giving up.
    pub retry_window_ms: u64,
    /// Sleep between retry rounds.
    pub retry_sleep_ms: u64,
}

#[cfg(feature = "balance")]
impl Default for FailoverOpts {
    fn default() -> Self {
        Self {
            // Enable background probing by default for failover so new connections can quickly
            // pick a healthy peer without waiting for long connect timeouts.
            probe_interval_ms: 2_000,
            probe_timeout_ms: 200,
            // Only used when peer health is unknown or stale.
            failfast_timeout_ms: 250,
            ok_ttl_ms: 6_000,
            backoff_base_ms: 500,
            backoff_max_ms: 30_000,
            retry_window_ms: 0,
            retry_sleep_ms: 200,
        }
    }
}

#[cfg(feature = "balance")]
impl FailoverOpts {
    pub fn sanitize(&mut self) {
        fn clamp_nonzero(v: &mut u64, min: u64, max: u64) {
            if *v == 0 {
                return;
            }
            *v = (*v).clamp(min, max);
        }

        // Prevent pathological busy loops / unbounded waits even with bad configs.
        clamp_nonzero(&mut self.probe_interval_ms, 200, 60_000);
        clamp_nonzero(&mut self.probe_timeout_ms, 50, 10_000);
        clamp_nonzero(&mut self.failfast_timeout_ms, 50, 10_000);
        clamp_nonzero(&mut self.ok_ttl_ms, 200, 120_000);
        clamp_nonzero(&mut self.backoff_base_ms, 50, 10_000);
        clamp_nonzero(&mut self.backoff_max_ms, 100, 600_000);
        if self.backoff_max_ms > 0 && self.backoff_base_ms > 0 {
            self.backoff_max_ms = self.backoff_max_ms.max(self.backoff_base_ms);
        }

        // Connection-level retry must remain bounded.
        self.retry_window_ms = self.retry_window_ms.min(600_000);
        if self.retry_window_ms > 0 {
            self.retry_sleep_ms = self.retry_sleep_ms.clamp(10, 10_000);
        }
    }
}

/// Remote address.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum RemoteAddr {
    SocketAddr(SocketAddr),
    DomainName(String, u16),
}

/// Proxy protocol options.
#[cfg(feature = "proxy")]
#[derive(Debug, Default, Clone, Copy)]
pub struct ProxyOpts {
    pub send_proxy: bool,
    pub accept_proxy: bool,
    pub send_proxy_version: usize,
    pub accept_proxy_timeout: usize,
}

#[cfg(feature = "proxy")]
impl ProxyOpts {
    #[inline]
    pub(crate) const fn enabled(&self) -> bool {
        self.send_proxy || self.accept_proxy
    }
}

/// Connect or associate options.
#[derive(Debug, Default, Clone)]
pub struct ConnectOpts {
    pub send_mptcp: bool,
    pub connect_timeout: usize,
    pub associate_timeout: usize,
    pub tcp_keepalive: usize,
    pub tcp_keepalive_probe: usize,
    pub bind_address: Option<SocketAddr>,
    pub bind_interface: Option<String>,

    #[cfg(feature = "proxy")]
    pub proxy_opts: ProxyOpts,

    #[cfg(feature = "transport")]
    pub transport: Option<(MixAccept, MixConnect)>,

    #[cfg(feature = "balance")]
    pub balancer: Balancer,

    #[cfg(feature = "balance")]
    pub failover: FailoverOpts,
}

#[derive(Debug, Default, Clone)]
pub struct BindOpts {
    pub ipv6_only: bool,
    pub accept_mptcp: bool,
    pub bind_interface: Option<String>,
}

/// Relay endpoint.
#[derive(Debug, Clone)]
pub struct Endpoint {
    pub laddr: SocketAddr,
    pub raddr: RemoteAddr,
    pub bind_opts: BindOpts,
    pub conn_opts: ConnectOpts,
    pub extra_raddrs: Vec<RemoteAddr>,
}

// display impl below

impl Display for RemoteAddr {
    fn fmt(&self, f: &mut Formatter<'_>) -> std::fmt::Result {
        use RemoteAddr::*;
        match self {
            SocketAddr(addr) => write!(f, "{}", addr),
            DomainName(host, port) => write!(f, "{}:{}", host, port),
        }
    }
}

impl Display for Endpoint {
    fn fmt(&self, f: &mut Formatter<'_>) -> std::fmt::Result {
        write!(f, "{} -> [{}", &self.laddr, &self.raddr)?;
        for raddr in self.extra_raddrs.iter() {
            write!(f, "|{}", raddr)?;
        }
        write!(f, "]; options: {}; {}", &self.bind_opts, &self.conn_opts)
    }
}

impl Display for BindOpts {
    fn fmt(&self, f: &mut Formatter<'_>) -> std::fmt::Result {
        let BindOpts {
            accept_mptcp,
            ipv6_only,
            bind_interface,
        } = self;
        if let Some(iface) = bind_interface {
            write!(f, "listen-iface={}, ", iface)?;
        }
        write!(f, "ipv6-only={}, ", ipv6_only)?;
        write!(f, "accept-mptcp={}", accept_mptcp)?;
        Ok(())
    }
}

impl Display for ConnectOpts {
    fn fmt(&self, f: &mut Formatter<'_>) -> std::fmt::Result {
        let ConnectOpts {
            send_mptcp,
            connect_timeout,
            associate_timeout,
            tcp_keepalive,
            tcp_keepalive_probe,
            bind_address,
            bind_interface,

            #[cfg(feature = "proxy")]
            proxy_opts,

            #[cfg(feature = "transport")]
            transport,

            #[cfg(feature = "balance")]
            balancer,

            #[cfg(feature = "balance")]
                failover: _,
            ..
        } = self;

        if let Some(iface) = bind_interface {
            write!(f, "send-iface={}, ", iface)?;
        }

        if let Some(send_through) = bind_address {
            write!(f, "send-through={}, ", send_through)?;
        }

        write!(f, "send-mptcp={}; ", send_mptcp)?;

        #[cfg(feature = "proxy")]
        {
            let ProxyOpts {
                send_proxy,
                accept_proxy,
                send_proxy_version,
                accept_proxy_timeout,
            } = proxy_opts;
            write!(
                f,
                "send-proxy={0}, send-proxy-version={2}, accept-proxy={1}, accept-proxy-timeout={3}s; ",
                send_proxy, accept_proxy, send_proxy_version, accept_proxy_timeout
            )?;
        }

        write!(
            f,
            "tcp-keepalive={}s[{}] connect-timeout={}s, associate-timeout={}s; ",
            tcp_keepalive, tcp_keepalive_probe, connect_timeout, associate_timeout
        )?;

        #[cfg(feature = "transport")]
        if let Some((ac, cc)) = transport {
            write!(f, "transport={}||{}; ", ac, cc)?;
        }

        #[cfg(feature = "balance")]
        write!(f, "balance={}", balancer.strategy())?;
        Ok(())
    }
}
