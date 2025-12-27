use std::net::IpAddr;
use std::sync::Arc;
use std::fmt::{Display, Formatter};

use crate::{Token, Balance};
use crate::failover::Failover;
use crate::ip_hash::IpHash;
use crate::round_robin::RoundRobin;

/// Balance strategy.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Strategy {
    Off,
    Failover,
    IpHash,
    RoundRobin,
}

impl From<&str> for Strategy {
    fn from(s: &str) -> Self {
        use Strategy::*;
        match s {
            "off" => Off,
            "failover" => Failover,
            "iphash" => IpHash,
            "roundrobin" => RoundRobin,
            _ => panic!("unknown strategy: {}", s),
        }
    }
}

impl Display for Strategy {
    fn fmt(&self, f: &mut Formatter<'_>) -> std::fmt::Result {
        match self {
            Strategy::Off => write!(f, "off"),
            Strategy::Failover => write!(f, "failover"),
            Strategy::IpHash => write!(f, "iphash"),
            Strategy::RoundRobin => write!(f, "roundrobin"),
        }
    }
}

/// Balance context to select next peer.
#[derive(Debug)]
pub struct BalanceCtx<'a> {
    pub src_ip: &'a IpAddr,
}

/// Combinated load balancer.
#[derive(Debug, Clone)]
pub enum Balancer {
    Off,
    Failover(Arc<Failover>),
    IpHash(Arc<IpHash>),
    RoundRobin(Arc<RoundRobin>),
}

impl Balancer {
    /// Constructor.
    pub fn new(strategy: Strategy, weights: &[u8]) -> Self {
        match strategy {
            Strategy::Off => Self::Off,
            Strategy::Failover => Self::Failover(Arc::new(Failover::new(weights))),
            Strategy::IpHash => Self::IpHash(Arc::new(IpHash::new(weights))),
            Strategy::RoundRobin => Self::RoundRobin(Arc::new(RoundRobin::new(weights))),
        }
    }

    /// Get current balance strategy.
    pub fn strategy(&self) -> Strategy {
        match self {
            Balancer::Off => Strategy::Off,
            Balancer::Failover(_) => Strategy::Failover,
            Balancer::IpHash(_) => Strategy::IpHash,
            Balancer::RoundRobin(_) => Strategy::RoundRobin,
        }
    }

    /// Get total peers.
    pub fn total(&self) -> u8 {
        match self {
            Balancer::Off => 0,
            Balancer::Failover(fo) => fo.total(),
            Balancer::IpHash(iphash) => iphash.total(),
            Balancer::RoundRobin(rr) => rr.total(),
        }
    }

    /// Select next peer.
    pub fn next(&self, ctx: BalanceCtx) -> Option<Token> {
        match self {
            Balancer::Off => Some(Token(0)),
            Balancer::Failover(fo) => fo.next(&()),
            Balancer::IpHash(iphash) => iphash.next(ctx.src_ip),
            Balancer::RoundRobin(rr) => rr.next(&()),
        }
    }

    /// Return candidate peers to try, in order.
    ///
    /// For `failover`, this returns all peers in priority order: 0, 1, 2, ...
    /// For other strategies, this returns a single selected peer.
    pub fn candidates(&self, ctx: BalanceCtx) -> Vec<Token> {
        match self {
            Balancer::Off => vec![Token(0)],
            Balancer::Failover(fo) => {
                let total = fo.total();
                if total == 0 {
                    vec![Token(0)]
                } else {
                    fo.order().collect()
                }
            }
            Balancer::IpHash(_) | Balancer::RoundRobin(_) => self.next(ctx).into_iter().collect(),
        }
    }

    /// Parse balancer from string.
    /// Format: $strategy: $weight1, $weight2, ...
    pub fn parse_from_str(s: &str) -> Self {
        let Some((strategy, weights)) = s.split_once(':') else {
            return Self::default();
        };

        let strategy = Strategy::from(strategy.trim());
        let weights: Vec<u8> = weights
            .trim()
            .split(',')
            .filter_map(|s| s.trim().parse().ok())
            .collect();

        Self::new(strategy, &weights)
    }

    pub fn try_parse_from_str(s: &str) -> Result<Self, String> {
        let (strategy, weights) = s
            .split_once(':')
            .ok_or_else(|| "invalid format, expected `$strategy: $weight1, $weight2, ...`".to_string())?;

        let strategy = Strategy::from(strategy.trim());
        let weights: Vec<u8> = weights
            .trim()
            .split(',')
            .map(|s| s.trim())
            .filter(|s| !s.is_empty())
            .map(|s| {
                s.parse::<u8>()
                    .map_err(|_| format!("invalid weight `{}` (expected 0-255 integer)", s))
            })
            .collect::<Result<Vec<u8>, String>>()?;

        Ok(Self::new(strategy, &weights))
    }
}

impl Default for Balancer {
    fn default() -> Self {
        Balancer::Off
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::net::IpAddr;

    #[test]
    fn parse_balancer() {
        fn run(strategy: Strategy, weights: &[u8]) {
            let mut s = String::with_capacity(128);
            s.push_str(&format!("{}: ", strategy));

            for weight in weights {
                s.push_str(&format!("{}, ", weight));
            }

            let balancer = Balancer::parse_from_str(&s);

            println!("balancer: {:?}", balancer);

            assert_eq!(balancer.strategy(), strategy);
            assert_eq!(balancer.total(), weights.len() as u8);
        }

        run(Strategy::Off, &[]);
        run(Strategy::Failover, &[]);
        run(Strategy::Failover, &[1, 1, 1]);
        run(Strategy::IpHash, &[]);
        run(Strategy::IpHash, &[1, 2, 3]);
        run(Strategy::IpHash, &[1, 2, 3]);
        run(Strategy::IpHash, &[1, 2, 3]);
        run(Strategy::RoundRobin, &[]);
        run(Strategy::RoundRobin, &[1, 2, 3]);
        run(Strategy::RoundRobin, &[1, 2, 3]);
        run(Strategy::RoundRobin, &[1, 2, 3]);
    }

    #[test]
    fn failover_candidates_are_in_order() {
        let balancer = Balancer::new(Strategy::Failover, &[9, 1, 1, 1]);
        let ip = "127.0.0.1".parse::<IpAddr>().unwrap();
        let tokens = balancer.candidates(BalanceCtx { src_ip: &ip });
        assert_eq!(tokens, vec![Token(0), Token(1), Token(2), Token(3)]);
    }
}
