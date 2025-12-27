use std::sync::atomic::{AtomicU32, AtomicU64, Ordering};
use std::time::Instant;

#[derive(Debug)]
struct PeerHealth {
    down_until_ms: AtomicU64,
    last_ok_ms: AtomicU64,
    fail_count: AtomicU32,
}

#[derive(Debug, Clone, Copy)]
pub struct FailoverPeerSnapshot {
    pub down_until_ms: u64,
    pub last_ok_ms: u64,
    pub fail_count: u32,
    pub should_skip: bool,
    pub ok_recent: bool,
}

/// Per-endpoint failover health state.
///
/// This is a lightweight circuit-breaker:
/// - after a connect failure, mark peer "down" for a short backoff window
/// - skip "down" peers when selecting a remote
/// - after backoff, the peer will be tried again (with a fail-fast timeout)
#[derive(Debug)]
pub struct FailoverHealth {
    start: Instant,
    peers: Vec<PeerHealth>,
    ok_ttl_ms: u64,
    backoff_base_ms: u64,
    backoff_max_ms: u64,
}

impl FailoverHealth {
    pub fn new(peer_count: usize, ok_ttl_ms: u64, backoff_base_ms: u64, backoff_max_ms: u64) -> Self {
        let peers = (0..peer_count)
            .map(|_| PeerHealth {
                down_until_ms: AtomicU64::new(0),
                last_ok_ms: AtomicU64::new(0),
                fail_count: AtomicU32::new(0),
            })
            .collect();
        Self {
            start: Instant::now(),
            peers,
            ok_ttl_ms,
            backoff_base_ms,
            backoff_max_ms,
        }
    }

    fn now_ms(&self) -> u64 {
        self.start.elapsed().as_millis() as u64
    }

    pub fn should_skip(&self, idx: u8) -> bool {
        let Some(peer) = self.peers.get(idx as usize) else {
            return false;
        };
        let now = self.now_ms();
        now < peer.down_until_ms.load(Ordering::Relaxed)
    }

    pub fn is_recent_ok(&self, idx: u8) -> bool {
        let Some(peer) = self.peers.get(idx as usize) else {
            return false;
        };
        let now = self.now_ms();
        let last_ok = peer.last_ok_ms.load(Ordering::Relaxed);
        last_ok != 0 && now.saturating_sub(last_ok) <= self.ok_ttl_ms
    }

    pub fn mark_ok(&self, idx: u8) {
        let Some(peer) = self.peers.get(idx as usize) else {
            return;
        };
        let now = self.now_ms();
        peer.last_ok_ms.store(now, Ordering::Relaxed);
        peer.down_until_ms.store(0, Ordering::Relaxed);
        peer.fail_count.store(0, Ordering::Relaxed);
    }

    pub fn mark_fail(&self, idx: u8) {
        let Some(peer) = self.peers.get(idx as usize) else {
            return;
        };
        let now = self.now_ms();

        // Once a peer fails, treat it as unhealthy until it succeeds again.
        peer.last_ok_ms.store(0, Ordering::Relaxed);

        let fail_count = peer.fail_count.fetch_add(1, Ordering::Relaxed).saturating_add(1);
        let exp = fail_count.min(16);
        let mut backoff = self.backoff_base_ms.saturating_mul(1u64 << exp);
        if backoff > self.backoff_max_ms {
            backoff = self.backoff_max_ms;
        }

        peer.down_until_ms.store(now.saturating_add(backoff), Ordering::Relaxed);
    }

    pub fn peer_snapshot(&self, idx: u8) -> Option<FailoverPeerSnapshot> {
        let Some(peer) = self.peers.get(idx as usize) else {
            return None;
        };
        let now = self.now_ms();
        let down_until_ms = peer.down_until_ms.load(Ordering::Relaxed);
        let last_ok_ms = peer.last_ok_ms.load(Ordering::Relaxed);
        let fail_count = peer.fail_count.load(Ordering::Relaxed);
        Some(FailoverPeerSnapshot {
            down_until_ms,
            last_ok_ms,
            fail_count,
            should_skip: now < down_until_ms,
            ok_recent: last_ok_ms != 0 && now.saturating_sub(last_ok_ms) <= self.ok_ttl_ms,
        })
    }

    pub fn peer_count(&self) -> usize {
        self.peers.len()
    }
}
