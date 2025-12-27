use super::{Balance, Token};

/// Failover balancer.
///
/// Always prefer the primary peer (Token(0)). If it is unavailable, the caller
/// should try the next candidates in order: Token(1), Token(2), ...
#[derive(Debug)]
pub struct Failover {
    total: u8,
}

impl Failover {
    pub fn order(&self) -> impl Iterator<Item = Token> + '_ {
        (0..self.total).map(Token)
    }
}

impl Balance for Failover {
    type State = ();

    fn new(weights: &[u8]) -> Self {
        assert!(weights.len() <= u8::MAX as usize);
        Self {
            total: weights.len() as u8,
        }
    }

    fn next(&self, _: &Self::State) -> Option<Token> {
        Some(Token(0))
    }

    fn total(&self) -> u8 {
        self.total
    }
}
