use std::io::Result;
use std::pin::Pin;
use std::sync::Arc;
use std::task::{Context, Poll};

use tokio::io::{AsyncRead, AsyncWrite};

use super::TcpObserver;

#[derive(Clone, Copy)]
pub enum CountDirection {
    Inbound,
    Outbound,
}

pub struct CountStream<T> {
    inner: T,
    observer: Arc<dyn TcpObserver>,
    id: u64,
    direction: CountDirection,
}

impl<T> CountStream<T> {
    pub fn new(inner: T, observer: Arc<dyn TcpObserver>, id: u64, direction: CountDirection) -> Self {
        Self {
            inner,
            observer,
            id,
            direction,
        }
    }
}

impl<T: AsyncRead + Unpin> AsyncRead for CountStream<T> {
    fn poll_read(self: Pin<&mut Self>, cx: &mut Context<'_>, buf: &mut tokio::io::ReadBuf<'_>) -> Poll<Result<()>> {
        let this = self.get_mut();
        Pin::new(&mut this.inner).poll_read(cx, buf)
    }
}

impl<T: AsyncWrite + Unpin> AsyncWrite for CountStream<T> {
    fn poll_write(self: Pin<&mut Self>, cx: &mut Context<'_>, buf: &[u8]) -> Poll<Result<usize>> {
        let this = self.get_mut();
        let res = Pin::new(&mut this.inner).poll_write(cx, buf);
        if let Poll::Ready(Ok(n)) = res {
            match this.direction {
                CountDirection::Inbound => this.observer.on_connection_bytes(this.id, n as u64, 0),
                CountDirection::Outbound => this.observer.on_connection_bytes(this.id, 0, n as u64),
            }
        }
        res
    }

    fn poll_flush(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Result<()>> {
        let this = self.get_mut();
        Pin::new(&mut this.inner).poll_flush(cx)
    }

    fn poll_shutdown(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Result<()>> {
        let this = self.get_mut();
        Pin::new(&mut this.inner).poll_shutdown(cx)
    }
}

#[cfg(target_os = "linux")]
impl<T: std::os::unix::io::AsRawFd> std::os::unix::io::AsRawFd for CountStream<T> {
    fn as_raw_fd(&self) -> std::os::unix::io::RawFd {
        self.inner.as_raw_fd()
    }
}

#[cfg(target_os = "linux")]
impl<T: realm_io::AsyncRawIO> realm_io::AsyncRawIO for CountStream<T> {
    #[inline]
    fn x_poll_read_ready(&self, cx: &mut Context<'_>) -> Poll<Result<()>> {
        self.inner.x_poll_read_ready(cx)
    }

    #[inline]
    fn x_poll_write_ready(&self, cx: &mut Context<'_>) -> Poll<Result<()>> {
        self.inner.x_poll_write_ready(cx)
    }

    #[inline]
    fn x_try_io<R>(&self, interest: tokio::io::Interest, f: impl FnOnce() -> Result<R>) -> Result<R> {
        self.inner.x_try_io(interest, f)
    }

    fn poll_write_raw<S>(&self, cx: &mut Context<'_>, syscall: S) -> Poll<Result<usize>>
    where
        S: FnMut() -> isize,
    {
        let res = self.inner.poll_write_raw(cx, syscall);
        if let Poll::Ready(Ok(n)) = res {
            if n > 0 {
                match self.direction {
                    CountDirection::Inbound => self.observer.on_connection_bytes(self.id, n as u64, 0),
                    CountDirection::Outbound => self.observer.on_connection_bytes(self.id, 0, n as u64),
                }
            }
        }
        res
    }
}
