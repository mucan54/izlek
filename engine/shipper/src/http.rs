//! Minimal HTTP listener for `/metrics` and `/healthz`.

use la_core::metrics::Registry;
use std::sync::Arc;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};

async fn handle(mut sock: TcpStream, registry: Arc<Registry>) -> std::io::Result<()> {
    let mut buf = vec![0u8; 4096];
    let mut len = 0;
    // Only the request line matters; stop at the end of the headers.
    while len < buf.len() {
        let n = sock.read(&mut buf[len..]).await?;
        if n == 0 {
            break;
        }
        len += n;
        if buf[..len].windows(4).any(|w| w == b"\r\n\r\n") {
            break;
        }
    }
    let head = String::from_utf8_lossy(&buf[..len]);
    let mut parts = head.lines().next().unwrap_or_default().split_whitespace();
    let (method, path) = (parts.next().unwrap_or_default(), parts.next().unwrap_or_default());
    let path = path.split('?').next().unwrap_or_default();
    let (status, ctype, body) = match (method, path) {
        ("GET", "/metrics") => ("200 OK", "text/plain; version=0.0.4", registry.render()),
        ("GET", "/healthz") => ("200 OK", "text/plain", "ok\n".to_owned()),
        ("GET", _) => ("404 Not Found", "text/plain", "not found\n".to_owned()),
        _ => ("405 Method Not Allowed", "text/plain", "method not allowed\n".to_owned()),
    };
    let resp = format!(
        "HTTP/1.1 {status}\r\nContent-Type: {ctype}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
        body.len()
    );
    sock.write_all(resp.as_bytes()).await?;
    sock.shutdown().await
}

/// Serves until the task is dropped. Returns the bound address.
pub async fn serve(addr: &str, registry: Arc<Registry>) -> std::io::Result<std::net::SocketAddr> {
    // ":9102" means every interface, as it does for the Go services.
    let addr = if addr.starts_with(':') { format!("0.0.0.0{addr}") } else { addr.to_owned() };
    let listener = TcpListener::bind(&addr).await?;
    let local = listener.local_addr()?;
    tokio::spawn(async move {
        loop {
            match listener.accept().await {
                Ok((sock, _)) => {
                    let registry = registry.clone();
                    tokio::spawn(async move {
                        let work = handle(sock, registry);
                        let _ = tokio::time::timeout(std::time::Duration::from_secs(10), work).await;
                    });
                }
                Err(e) => {
                    tracing::warn!(error = %e, "metrics listener accept failed");
                    tokio::time::sleep(std::time::Duration::from_millis(200)).await;
                }
            }
        }
    });
    Ok(local)
}

#[cfg(test)]
mod tests {
    use super::*;

    async fn get(addr: std::net::SocketAddr, req: &str) -> String {
        let mut s = TcpStream::connect(addr).await.unwrap();
        s.write_all(req.as_bytes()).await.unwrap();
        let mut out = String::new();
        s.read_to_string(&mut out).await.unwrap();
        out
    }

    #[tokio::test]
    async fn serves_metrics_and_health() {
        let reg = Arc::new(Registry::new("la_shipper_"));
        reg.inc("records_out_total", &[], 7);
        let addr = serve("127.0.0.1:0", reg).await.unwrap();
        let m = get(addr, "GET /metrics HTTP/1.1\r\nHost: x\r\n\r\n").await;
        assert!(m.starts_with("HTTP/1.1 200 OK"), "{m}");
        assert!(m.contains("la_shipper_records_out_total 7\n"), "{m}");
        assert!(get(addr, "GET /healthz HTTP/1.0\r\n\r\n").await.ends_with("ok\n"));
        assert!(get(addr, "GET /nope HTTP/1.1\r\n\r\n").await.starts_with("HTTP/1.1 404"));
        assert!(get(addr, "POST /metrics HTTP/1.1\r\n\r\n").await.starts_with("HTTP/1.1 405"));
    }
}
