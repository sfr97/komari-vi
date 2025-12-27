use std::io::Result;
use tokio::io::{AsyncRead, AsyncWrite};

#[inline]
#[cfg(target_os = "linux")]
pub async fn run_relay<A, B>(mut local: A, mut remote: B) -> Result<()>
where
    A: AsyncRead + AsyncWrite + realm_io::AsyncRawIO + Unpin,
    B: AsyncRead + AsyncWrite + realm_io::AsyncRawIO + Unpin,
{
    use std::io::ErrorKind;
    match realm_io::bidi_zero_copy(&mut local, &mut remote).await {
        Ok(_) => Ok(()),
        Err(ref e) if e.kind() == ErrorKind::InvalidInput => {
            realm_io::bidi_copy(&mut local, &mut remote).await.map(|_| ())
        }
        Err(e) => Err(e),
    }
}

#[inline]
#[cfg(not(target_os = "linux"))]
pub async fn run_relay<A, B>(mut local: A, mut remote: B) -> Result<()>
where
    A: AsyncRead + AsyncWrite + Unpin,
    B: AsyncRead + AsyncWrite + Unpin,
{
    realm_io::bidi_copy(&mut local, &mut remote).await.map(|_| ())
}
