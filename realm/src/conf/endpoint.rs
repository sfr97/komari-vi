use serde::{Serialize, Deserialize};
use std::net::{IpAddr, SocketAddr, ToSocketAddrs};
use std::{error::Error, fmt};

use realm_core::endpoint::{Endpoint, RemoteAddr};

#[cfg(feature = "balance")]
use realm_core::balance::Balancer;
#[cfg(feature = "balance")]
use realm_core::balance::Strategy;

#[cfg(feature = "transport")]
use realm_core::kaminari::mix::{MixAccept, MixConnect};

use super::{Config, NetConf, NetInfo};

#[derive(Debug)]
pub enum EndpointBuildError {
    InvalidListen(String),
    InvalidRemote(String),
    InvalidThrough(String),
    InvalidBalance(String),
    NoTransportEnabled,
}

impl fmt::Display for EndpointBuildError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            EndpointBuildError::InvalidListen(msg) => write!(f, "invalid `listen`: {}", msg),
            EndpointBuildError::InvalidRemote(msg) => write!(f, "invalid `remote`: {}", msg),
            EndpointBuildError::InvalidThrough(msg) => write!(f, "invalid `through`: {}", msg),
            EndpointBuildError::InvalidBalance(msg) => write!(f, "invalid `balance`: {}", msg),
            EndpointBuildError::NoTransportEnabled => write!(f, "invalid `network`: both tcp and udp are disabled"),
        }
    }
}

impl Error for EndpointBuildError {}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct EndpointConf {
    pub listen: String,

    pub remote: String,

    #[serde(default)]
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub extra_remotes: Vec<String>,

    #[serde(default)]
    #[serde(skip_serializing_if = "Option::is_none")]
    pub balance: Option<String>,

    #[serde(default)]
    #[serde(skip_serializing_if = "Option::is_none")]
    pub through: Option<String>,

    #[serde(default)]
    #[serde(skip_serializing_if = "Option::is_none")]
    pub interface: Option<String>,

    #[serde(default)]
    #[serde(skip_serializing_if = "Option::is_none")]
    pub listen_interface: Option<String>,

    #[serde(default)]
    #[serde(skip_serializing_if = "Option::is_none")]
    pub listen_transport: Option<String>,

    #[serde(default)]
    #[serde(skip_serializing_if = "Option::is_none")]
    pub remote_transport: Option<String>,

    #[serde(default)]
    #[serde(skip_serializing_if = "Config::is_empty")]
    pub network: NetConf,
}

impl EndpointConf {
    fn try_build_local(&self) -> Result<SocketAddr, EndpointBuildError> {
        let mut addrs = self
            .listen
            .to_socket_addrs()
            .map_err(|e| EndpointBuildError::InvalidListen(e.to_string()))?;
        addrs
            .next()
            .ok_or_else(|| EndpointBuildError::InvalidListen("no address resolved".to_string()))
    }

    fn try_build_remote(&self) -> Result<RemoteAddr, EndpointBuildError> {
        Self::try_build_remote_x(&self.remote)
    }

    fn try_build_remote_x(remote: &str) -> Result<RemoteAddr, EndpointBuildError> {
        if let Ok(sockaddr) = remote.parse::<SocketAddr>() {
            return Ok(RemoteAddr::SocketAddr(sockaddr));
        }

        let mut iter = remote.rsplitn(2, ':');
        let port_str = iter
            .next()
            .ok_or_else(|| EndpointBuildError::InvalidRemote("missing port".to_string()))?;
        let host = iter
            .next()
            .ok_or_else(|| EndpointBuildError::InvalidRemote("missing host".to_string()))?;

        let port = port_str
            .parse::<u16>()
            .map_err(|_| EndpointBuildError::InvalidRemote(format!("invalid port `{}`", port_str)))?;

        if host.is_empty() {
            return Err(EndpointBuildError::InvalidRemote("empty host".to_string()));
        }

        Ok(RemoteAddr::DomainName(host.to_string(), port))
    }

    fn try_build_send_through(&self) -> Result<Option<SocketAddr>, EndpointBuildError> {
        let Self { through, .. } = self;
        let through = match through {
            Some(x) => x,
            None => return Ok(None),
        };
        match through.to_socket_addrs() {
            Ok(mut x) => x
                .next()
                .ok_or_else(|| EndpointBuildError::InvalidThrough("no address resolved".to_string()))
                .map(Some),
            Err(_) => {
                let mut ipstr = String::from(through);
                ipstr.retain(|c| c != '[' && c != ']');
                ipstr
                    .parse::<IpAddr>()
                    .map(|ip| Some(SocketAddr::new(ip, 0)))
                    .map_err(|e| EndpointBuildError::InvalidThrough(e.to_string()))
            }
        }
    }

    #[cfg(feature = "balance")]
    fn try_build_balancer(&self) -> Result<Balancer, EndpointBuildError> {
        if let Some(s) = &self.balance {
            let (strategy, weights) = match s.split_once(':') {
                Some((strategy, weights)) => (strategy, weights),
                None => (s.as_str(), ""),
            };

            let strategy = match strategy.trim().to_ascii_lowercase().as_str() {
                "off" => Strategy::Off,
                "failover" => Strategy::Failover,
                "iphash" => Strategy::IpHash,
                "roundrobin" => Strategy::RoundRobin,
                other => {
                    return Err(EndpointBuildError::InvalidBalance(format!(
                        "unknown strategy `{}` (expected one of: off, failover, iphash, roundrobin)",
                        other
                    )))
                }
            };

            let mut parsed_weights: Vec<u8> = Vec::new();
            for w in weights.trim().split(',').map(|s| s.trim()).filter(|s| !s.is_empty()) {
                let w = w.parse::<u8>().map_err(|_| {
                    EndpointBuildError::InvalidBalance(format!("invalid weight `{}` (expected 0-255 integer)", w))
                })?;
                parsed_weights.push(w);
            }

            if strategy == Strategy::Failover {
                let expected = 1 + self.extra_remotes.len();
                if parsed_weights.is_empty() {
                    parsed_weights.resize(expected, 1);
                } else if parsed_weights.len() != expected {
                    return Err(EndpointBuildError::InvalidBalance(format!(
                        "failover requires {} weights (remote + extra_remotes), got {}",
                        expected,
                        parsed_weights.len()
                    )));
                } else {
                    let primary = parsed_weights[0];
                    let backup_max = parsed_weights[1..].iter().copied().max().unwrap_or(0);
                    if primary < backup_max {
                        return Err(EndpointBuildError::InvalidBalance(
                            "failover requires `remote` to have the highest weight".to_string(),
                        ));
                    }
                }
            }

            Ok(Balancer::new(strategy, &parsed_weights))
        } else {
            Ok(Balancer::default())
        }
    }

    #[cfg(feature = "transport")]
    fn build_transport(&self) -> Option<(MixAccept, MixConnect)> {
        use realm_core::kaminari::mix::{MixClientConf, MixServerConf};
        use realm_core::kaminari::opt::get_ws_conf;
        use realm_core::kaminari::opt::get_tls_client_conf;
        use realm_core::kaminari::opt::get_tls_server_conf;

        let Self {
            listen_transport,
            remote_transport,
            ..
        } = self;

        let listen_ws = listen_transport.as_ref().and_then(|s| get_ws_conf(s));
        let listen_tls = listen_transport.as_ref().and_then(|s| get_tls_server_conf(s));

        let remote_ws = remote_transport.as_ref().and_then(|s| get_ws_conf(s));
        let remote_tls = remote_transport.as_ref().and_then(|s| get_tls_client_conf(s));

        if matches!(
            (&listen_ws, &listen_tls, &remote_ws, &remote_tls),
            (None, None, None, None)
        ) {
            None
        } else {
            let ac = MixAccept::new_shared(MixServerConf {
                ws: listen_ws,
                tls: listen_tls,
            });
            let cc = MixConnect::new_shared(MixClientConf {
                ws: remote_ws,
                tls: remote_tls,
            });
            Some((ac, cc))
        }
    }

    pub fn try_build(self) -> Result<EndpointInfo, EndpointBuildError> {
        let laddr = self.try_build_local()?;
        let raddr = self.try_build_remote()?;

        let extra_raddrs = self
            .extra_remotes
            .iter()
            .map(|r| Self::try_build_remote_x(r))
            .collect::<Result<Vec<_>, _>>()?;

        let NetInfo {
            mut bind_opts,
            mut conn_opts,
            no_tcp,
            use_udp,
        } = self.network.build();

        if no_tcp && !use_udp {
            return Err(EndpointBuildError::NoTransportEnabled);
        }

        #[cfg(feature = "balance")]
        {
            conn_opts.balancer = self.try_build_balancer()?;
        }

        #[cfg(feature = "transport")]
        {
            conn_opts.transport = self.build_transport();
        }

        conn_opts.bind_address = self.try_build_send_through()?;
        conn_opts.bind_interface = self.interface;
        bind_opts.bind_interface = self.listen_interface;

        Ok(EndpointInfo {
            no_tcp,
            use_udp,
            endpoint: Endpoint {
                laddr,
                raddr,
                bind_opts,
                conn_opts,
                extra_raddrs,
            },
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn invalid_remote_missing_host_returns_error() {
        let conf = EndpointConf {
            listen: "127.0.0.1:0".to_string(),
            remote: "example.com".to_string(),
            extra_remotes: vec![],
            balance: None,
            through: None,
            interface: None,
            listen_interface: None,
            listen_transport: None,
            remote_transport: None,
            network: Default::default(),
        };

        let err = conf.try_build().unwrap_err();
        let msg = err.to_string();
        assert!(msg.contains("invalid `remote`"));
        assert!(msg.contains("missing host"));
    }

    #[test]
    fn invalid_remote_empty_host_returns_error() {
        let conf = EndpointConf {
            listen: "127.0.0.1:0".to_string(),
            remote: ":80".to_string(),
            extra_remotes: vec![],
            balance: None,
            through: None,
            interface: None,
            listen_interface: None,
            listen_transport: None,
            remote_transport: None,
            network: Default::default(),
        };

        let err = conf.try_build().unwrap_err();
        let msg = err.to_string();
        assert!(msg.contains("invalid `remote`"));
        assert!(msg.contains("empty host"));
    }

    #[test]
    fn invalid_remote_bad_port_returns_error() {
        let conf = EndpointConf {
            listen: "127.0.0.1:0".to_string(),
            remote: "example.com:99999".to_string(),
            extra_remotes: vec![],
            balance: None,
            through: None,
            interface: None,
            listen_interface: None,
            listen_transport: None,
            remote_transport: None,
            network: Default::default(),
        };

        let err = conf.try_build().unwrap_err();
        let msg = err.to_string();
        assert!(msg.contains("invalid `remote`"));
        assert!(msg.contains("invalid port"));
    }

    #[test]
    fn invalid_through_returns_error() {
        let conf = EndpointConf {
            listen: "127.0.0.1:0".to_string(),
            remote: "example.com:80".to_string(),
            extra_remotes: vec![],
            balance: None,
            through: Some("not-an-addr".to_string()),
            interface: None,
            listen_interface: None,
            listen_transport: None,
            remote_transport: None,
            network: Default::default(),
        };

        let err = conf.try_build().unwrap_err();
        let msg = err.to_string();
        assert!(msg.contains("invalid `through`"));
    }

    #[test]
    #[cfg(feature = "balance")]
    fn balance_unknown_strategy_returns_error_instead_of_panic() {
        let conf = EndpointConf {
            listen: "127.0.0.1:0".to_string(),
            remote: "example.com:80".to_string(),
            extra_remotes: vec![],
            balance: Some("unknown: 1,2,3".to_string()),
            through: None,
            interface: None,
            listen_interface: None,
            listen_transport: None,
            remote_transport: None,
            network: Default::default(),
        };

        let err = conf.try_build().unwrap_err();
        let msg = err.to_string();
        assert!(msg.contains("invalid `balance`"));
        assert!(msg.contains("unknown strategy"));
    }

    #[test]
    #[cfg(feature = "balance")]
    fn balance_failover_without_weights_infers_peer_count() {
        let conf = EndpointConf {
            listen: "127.0.0.1:0".to_string(),
            remote: "example.com:80".to_string(),
            extra_remotes: vec!["example.org:80".to_string(), "example.net:80".to_string()],
            balance: Some("failover".to_string()),
            through: None,
            interface: None,
            listen_interface: None,
            listen_transport: None,
            remote_transport: None,
            network: Default::default(),
        };

        let info = conf.try_build().unwrap();
        assert_eq!(info.endpoint.conn_opts.balancer.strategy(), Strategy::Failover);
        assert_eq!(info.endpoint.conn_opts.balancer.total(), 3);
    }

    #[test]
    #[cfg(feature = "balance")]
    fn balance_failover_requires_remote_highest_weight() {
        let conf = EndpointConf {
            listen: "127.0.0.1:0".to_string(),
            remote: "example.com:80".to_string(),
            extra_remotes: vec!["example.org:80".to_string(), "example.net:80".to_string()],
            balance: Some("failover: 1, 2, 1".to_string()),
            through: None,
            interface: None,
            listen_interface: None,
            listen_transport: None,
            remote_transport: None,
            network: Default::default(),
        };

        let err = conf.try_build().unwrap_err();
        let msg = err.to_string();
        assert!(msg.contains("invalid `balance`"));
        assert!(msg.contains("highest weight"));
    }

    #[test]
    fn invalid_listen_returns_error() {
        let conf = EndpointConf {
            listen: "not-a-socket-addr".to_string(),
            remote: "example.com:80".to_string(),
            extra_remotes: vec![],
            balance: None,
            through: None,
            interface: None,
            listen_interface: None,
            listen_transport: None,
            remote_transport: None,
            network: Default::default(),
        };

        let err = conf.try_build().unwrap_err();
        assert!(matches!(err, EndpointBuildError::InvalidListen(_)));
    }
}

#[derive(Debug)]
pub struct EndpointInfo {
    pub no_tcp: bool,
    pub use_udp: bool,
    pub endpoint: Endpoint,
}

impl Config for EndpointConf {
    type Output = EndpointInfo;

    fn is_empty(&self) -> bool {
        false
    }

    fn build(self) -> Self::Output {
        self.try_build().unwrap_or_else(|e| panic!("{}", e))
    }

    fn rst_field(&mut self, _: &Self) -> &mut Self {
        unreachable!()
    }

    fn take_field(&mut self, _: &Self) -> &mut Self {
        unreachable!()
    }

    fn from_cmd_args(matches: &clap::ArgMatches) -> Self {
        let listen = matches.get_one("local").cloned().unwrap();
        let remote = matches.get_one("remote").cloned().unwrap();
        let through = matches.get_one("through").cloned();
        let interface = matches.get_one("interface").cloned();
        let listen_interface = matches.get_one("listen_interface").cloned();
        let listen_transport = matches.get_one("listen_transport").cloned();
        let remote_transport = matches.get_one("remote_transport").cloned();

        EndpointConf {
            listen,
            remote,
            through,
            interface,
            listen_interface,
            listen_transport,
            remote_transport,
            network: Default::default(),
            extra_remotes: Vec::new(),
            balance: None,
        }
    }
}
