//! quicperf: plain quinn throughput, one connection, one stream, no Nu.
//!
//!   quicperf listen 0.0.0.0:4997 [gso|nogso]
//!   quicperf dial 192.168.1.200:4997 <bytes> [gso|nogso]
use std::{env, net::SocketAddr, sync::Arc, time::Instant};

use quinn::{ClientConfig, Endpoint, ServerConfig, TransportConfig};
use rustls::pki_types::{CertificateDer, PrivatePkcs8KeyDer, ServerName, UnixTime};

fn transport(gso: bool) -> Arc<TransportConfig> {
    let mut t = TransportConfig::default();
    t.enable_segmentation_offload(gso);
    Arc::new(t)
}

#[tokio::main]
async fn main() {
    let args: Vec<String> = env::args().collect();
    let addr: SocketAddr = args[2].parse().unwrap();
    let gso = args.last().map(|s| s != "nogso").unwrap_or(true);
    if args[1] == "listen" {
        let cert = rcgen::generate_simple_self_signed(vec!["quicperf".into()]).unwrap();
        let key = PrivatePkcs8KeyDer::from(cert.key_pair.serialize_der());
        let mut sc = ServerConfig::with_single_cert(vec![CertificateDer::from(cert.cert)], key.into()).unwrap();
        sc.transport_config(transport(gso));
        let ep = Endpoint::server(sc, addr).unwrap();
        while let Some(inc) = ep.accept().await {
            tokio::spawn(async move {
                let conn = inc.await.unwrap();
                while let Ok((mut send, mut recv)) = conn.accept_bi().await {
                    tokio::spawn(async move {
                        let mut buf = vec![0u8; 1 << 20];
                        while let Some(_) = recv.read(&mut buf).await.unwrap() {}
                        send.write_all(b"k").await.unwrap();
                        send.finish().unwrap();
                        let _ = send.stopped().await;
                    });
                }
            });
        }
        return;
    }
    let total: u64 = args[3].parse().unwrap();
    let provider = Arc::new(rustls::crypto::ring::default_provider());
    let crypto = rustls::ClientConfig::builder_with_provider(provider.clone())
        .with_protocol_versions(&[&rustls::version::TLS13])
        .unwrap()
        .dangerous()
        .with_custom_certificate_verifier(Arc::new(SkipVerify(provider)))
        .with_no_client_auth();
    let mut cc = ClientConfig::new(Arc::new(quinn::crypto::rustls::QuicClientConfig::try_from(crypto).unwrap()));
    cc.transport_config(transport(gso));
    let mut ep = Endpoint::client("0.0.0.0:0".parse().unwrap()).unwrap();
    ep.set_default_client_config(cc);
    let conn = ep.connect(addr, "quicperf").unwrap().await.unwrap();
    let buf = vec![0x5au8; 64 << 10];
    let start = Instant::now();
    let (mut send, mut recv) = conn.open_bi().await.unwrap();
    let mut sent = 0u64;
    while sent < total {
        send.write_all(&buf).await.unwrap();
        sent += buf.len() as u64;
    }
    send.finish().unwrap();
    recv.read_to_end(16).await.unwrap(); // the server's ack after the last byte
    let secs = start.elapsed().as_secs_f64();
    println!("quinn gso={gso}: {:.0} MB/s ({:.1} s)", sent as f64 / 1e6 / secs, secs);
    conn.close(0u32.into(), b"done");
    ep.wait_idle().await;
}

#[derive(Debug)]
struct SkipVerify(Arc<rustls::crypto::CryptoProvider>);

impl rustls::client::danger::ServerCertVerifier for SkipVerify {
    fn verify_server_cert(&self, _: &CertificateDer<'_>, _: &[CertificateDer<'_>], _: &ServerName<'_>, _: &[u8], _: UnixTime)
        -> Result<rustls::client::danger::ServerCertVerified, rustls::Error> {
        Ok(rustls::client::danger::ServerCertVerified::assertion())
    }
    fn verify_tls12_signature(&self, m: &[u8], c: &CertificateDer<'_>, d: &rustls::DigitallySignedStruct)
        -> Result<rustls::client::danger::HandshakeSignatureValid, rustls::Error> {
        rustls::crypto::verify_tls12_signature(m, c, d, &self.0.signature_verification_algorithms)
    }
    fn verify_tls13_signature(&self, m: &[u8], c: &CertificateDer<'_>, d: &rustls::DigitallySignedStruct)
        -> Result<rustls::client::danger::HandshakeSignatureValid, rustls::Error> {
        rustls::crypto::verify_tls13_signature(m, c, d, &self.0.signature_verification_algorithms)
    }
    fn supported_verify_schemes(&self) -> Vec<rustls::SignatureScheme> {
        self.0.signature_verification_algorithms.supported_schemes()
    }
}
