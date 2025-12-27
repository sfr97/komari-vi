use axum::{
    extract::{Path, State},
    http::{StatusCode, HeaderMap},
    response::Json,
    routing::{delete, get, patch, post, put},
    Router,
    middleware::from_fn_with_state,
};
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::Instant;
use std::{env, fs, net::SocketAddr, path::Path as StdPath};
use tokio::sync::{Mutex as AsyncMutex, oneshot};
use tokio::task::{AbortHandle, JoinHandle};
use tokio::time::{timeout, Duration};
use chrono::Utc;

use headers::HeaderName;

use crate::conf::{Config, EndpointConf, EndpointInfo, FullConf, PersistedInstance};

use realm_core::tcp::TcpObserver;
use realm_core::udp::UdpObserver;

pub const ENV_API_KEY: &str = "REALM_API_KEY";

static X_API_KEY: HeaderName = HeaderName::from_static("x-api-key");

async fn auth_middleware(
    State(state): State<AppState>,
    headers: HeaderMap,
    request: axum::extract::Request,
    next: axum::middleware::Next,
) -> Result<axum::response::Response, (StatusCode, Json<ApiErrorResponse>)> {
    if is_request_authorized(state.api_key.as_deref(), &headers) {
        return Ok(next.run(request).await);
    }

    Err((
        StatusCode::UNAUTHORIZED,
        api_error("unauthorized", "missing or invalid X-API-Key"),
    ))
}

fn is_request_authorized(expected_key: Option<&str>, headers: &HeaderMap) -> bool {
    let Some(expected_key) = expected_key else {
        return true;
    };

    let Some(api_key_header) = headers.get(&X_API_KEY) else {
        return false;
    };
    let Ok(provided_key) = api_key_header.to_str() else {
        return false;
    };

    provided_key == expected_key
}

#[derive(Serialize)]
pub struct ApiErrorResponse {
    pub error: ApiError,
}

#[derive(Serialize)]
pub struct ApiError {
    pub code: &'static str,
    pub message: String,
}

type ApiResult<T> = Result<T, (StatusCode, Json<ApiErrorResponse>)>;

fn api_error(code: &'static str, message: impl Into<String>) -> Json<ApiErrorResponse> {
    Json(ApiErrorResponse {
        error: ApiError {
            code,
            message: message.into(),
        },
    })
}

fn now_rfc3339() -> String {
    Utc::now().to_rfc3339()
}

fn build_backend_aggregates(
    stats: &InstanceStats,
    default_backend: &str,
) -> (HashMap<String, u64>, HashMap<String, BackendBytes>) {
    let mut connections_by_backend: HashMap<String, u64> = HashMap::new();

    {
        let conns = match stats.connections.lock() {
            Ok(x) => x,
            Err(e) => e.into_inner(),
        };
        for entry in conns.values() {
            let backend = entry.backend.clone().unwrap_or_else(|| default_backend.to_string());
            *connections_by_backend.entry(backend.clone()).or_default() += 1;
        }
    }

    let mut bytes_by_backend: HashMap<String, BackendBytes> = match stats.tcp_bytes_by_backend.lock() {
        Ok(x) => x.clone(),
        Err(e) => e.into_inner().clone(),
    };

    let udp_current = match stats.udp_sessions.lock() {
        Ok(x) => x.len() as u64,
        Err(e) => e.into_inner().len() as u64,
    };
    if udp_current > 0 {
        *connections_by_backend.entry(default_backend.to_string()).or_default() += udp_current;
    }

    let udp_in = stats.udp_inbound_bytes.load(Ordering::Relaxed);
    let udp_out = stats.udp_outbound_bytes.load(Ordering::Relaxed);
    if udp_in > 0 || udp_out > 0 {
        let bb = bytes_by_backend.entry(default_backend.to_string()).or_default();
        bb.inbound_bytes = bb.inbound_bytes.saturating_add(udp_in);
        bb.outbound_bytes = bb.outbound_bytes.saturating_add(udp_out);
    }

    (connections_by_backend, bytes_by_backend)
}

#[derive(Clone, Serialize, Deserialize)]
pub struct Instance {
    pub id: String,
    pub config: EndpointConf,
    pub status: InstanceStatus,
    #[serde(default = "default_auto_start")]
    pub auto_start: bool,
}

fn default_auto_start() -> bool {
    true
}

#[derive(Serialize, Deserialize)]
pub struct InstanceAutoStartUpdate {
    pub auto_start: bool,
}

#[derive(Clone, Serialize, Deserialize)]
pub enum InstanceStatus {
    Running,
    Stopped,
    Failed(String),
}

#[derive(Clone)]
pub enum PersistenceMode {
    Hybrid {
        config_file: String,
        format: PersistFormat,
    },
    SelfManaged {
        storage_path: String,
        format: PersistFormat,
    },
}

#[derive(Clone, Copy)]
pub enum PersistFormat {
    Json,
    Toml,
}

impl PersistFormat {
    fn from_path(path: &str) -> PersistFormat {
        if StdPath::new(path)
            .extension()
            .is_some_and(|ext| ext.eq_ignore_ascii_case("toml"))
        {
            PersistFormat::Toml
        } else {
            PersistFormat::Json
        }
    }
}

#[derive(Clone)]
pub struct PersistenceManager {
    mode: PersistenceMode,
    global_config: Option<FullConf>,
    write_lock: Arc<AsyncMutex<()>>,
}

impl PersistenceManager {
    pub fn new(config_file: Option<String>, global_config: Option<FullConf>) -> Self {
        let mode = match config_file {
            Some(file) => PersistenceMode::Hybrid {
                format: PersistFormat::from_path(&file),
                config_file: file,
            },
            None => {
                let storage_path =
                    env::var("REALM_INSTANCE_STORE").unwrap_or_else(|_| "./instances/realm.json".to_string());
                PersistenceMode::SelfManaged {
                    format: PersistFormat::from_path(&storage_path),
                    storage_path,
                }
            }
        };

        PersistenceManager {
            mode,
            global_config,
            write_lock: Arc::new(AsyncMutex::new(())),
        }
    }

    pub async fn save_instances(
        &self,
        instances: &HashMap<String, InstanceData>,
    ) -> Result<(), Box<dyn std::error::Error>> {
        let _lock = self.write_lock.lock().await;

        let persisted_instances: Vec<PersistedInstance> = instances
            .values()
            .map(|data| PersistedInstance {
                id: data.instance.id.clone(),
                config: data.instance.config.clone(),
                status: match &data.instance.status {
                    InstanceStatus::Running => "Running".to_string(),
                    InstanceStatus::Stopped => "Stopped".to_string(),
                    InstanceStatus::Failed(e) => format!("Failed({})", e),
                },
                auto_start: data.instance.auto_start,
                created_at: data.created_at.clone(),
                updated_at: data.updated_at.clone(),
            })
            .collect();

        match &self.mode {
            PersistenceMode::Hybrid { config_file, format } => {
                self.save_hybrid_config(config_file, *format, persisted_instances).await
            }
            PersistenceMode::SelfManaged { storage_path, format } => {
                self.save_self_managed_config(storage_path, *format, persisted_instances)
                    .await
            }
        }
    }

    fn create_instances_snapshot(instances: &HashMap<String, InstanceData>) -> HashMap<String, InstanceData> {
        instances
            .iter()
            .map(|(k, v)| {
                (
                    k.clone(),
                    InstanceData {
                        instance: v.instance.clone(),
                        tcp_abort: None,
                        udp_abort: None,
                        generation: v.generation,
                        created_at: v.created_at.clone(),
                        updated_at: v.updated_at.clone(),
                        stats: v.stats.clone(),
                    },
                )
            })
            .collect()
    }

    async fn save_hybrid_config(
        &self,
        config_file: &str,
        format: PersistFormat,
        instances: Vec<PersistedInstance>,
    ) -> Result<(), Box<dyn std::error::Error>> {
        let mut config = if StdPath::new(config_file).exists() {
            FullConf::from_conf_file(config_file)
        } else {
            self.global_config.clone().unwrap_or_default()
        };

        config.instances = instances;

        let content = match format {
            PersistFormat::Toml => toml::to_string_pretty(&config)?,
            PersistFormat::Json => serde_json::to_string_pretty(&config)?,
        };

        self.atomic_write(config_file, content).await?;
        Ok(())
    }

    async fn save_self_managed_config(
        &self,
        storage_path: &str,
        format: PersistFormat,
        instances: Vec<PersistedInstance>,
    ) -> Result<(), Box<dyn std::error::Error>> {
        let config = FullConf {
            log: self.create_default_log_config(),
            dns: self.create_default_dns_config(),
            network: self.create_default_network_config(),
            endpoints: vec![],
            instances,
        };

        if let Some(parent) = StdPath::new(storage_path).parent() {
            fs::create_dir_all(parent)?;
        }

        let content = match format {
            PersistFormat::Toml => toml::to_string_pretty(&config)?,
            PersistFormat::Json => serde_json::to_string_pretty(&config)?,
        };

        self.atomic_write(storage_path, content).await?;
        Ok(())
    }

    async fn atomic_write(&self, file_path: &str, content: String) -> std::io::Result<()> {
        let file_path = file_path.to_string();
        tokio::task::spawn_blocking(move || -> std::io::Result<()> {
            use std::io::Write;

            let temp_file = format!("{}.tmp", file_path);
            if let Some(parent) = StdPath::new(&file_path).parent() {
                std::fs::create_dir_all(parent)?;
            }

            {
                let mut f = std::fs::File::create(&temp_file)?;
                f.write_all(content.as_bytes())?;
                f.sync_all()?;
            }

            match std::fs::rename(&temp_file, &file_path) {
                Ok(()) => Ok(()),
                Err(e) => {
                    if StdPath::new(&file_path).exists() {
                        let _ = std::fs::remove_file(&file_path);
                        std::fs::rename(&temp_file, &file_path)?;
                        Ok(())
                    } else {
                        Err(e)
                    }
                }
            }
        })
        .await
        .map_err(|e| std::io::Error::new(std::io::ErrorKind::Other, e.to_string()))??;

        Ok(())
    }

    pub fn load_instances(&self) -> Result<Vec<PersistedInstance>, Box<dyn std::error::Error>> {
        let config_path = match &self.mode {
            PersistenceMode::Hybrid { config_file, .. } => config_file.clone(),
            PersistenceMode::SelfManaged { storage_path, .. } => storage_path.clone(),
        };

        if !StdPath::new(&config_path).exists() {
            return Ok(vec![]);
        }

        let config = FullConf::from_conf_file(&config_path);
        Ok(config.instances)
    }

    fn create_default_log_config(&self) -> crate::conf::LogConf {
        crate::conf::LogConf::default()
    }

    fn create_default_dns_config(&self) -> crate::conf::DnsConf {
        crate::conf::DnsConf::default()
    }

    fn create_default_network_config(&self) -> crate::conf::NetConf {
        crate::conf::NetConf::default()
    }
}

#[derive(Clone)]
pub struct AppState {
    pub instances: Arc<AsyncMutex<HashMap<String, InstanceData>>>,
    pub api_key: Option<String>,
    pub global_config: Option<FullConf>,
    pub persistence: Option<PersistenceManager>,
    pub endpoint_starter: EndpointStarter,
}

type EndpointStartFuture =
    Pin<Box<dyn Future<Output = Result<(Option<AbortHandle>, Option<AbortHandle>), String>> + Send>>;

pub type EndpointStarter = Arc<
    dyn Fn(
            Arc<AsyncMutex<HashMap<String, InstanceData>>>,
            Option<PersistenceManager>,
            String,
            u64,
            EndpointInfo,
        ) -> EndpointStartFuture
        + Send
        + Sync,
>;

fn default_endpoint_starter() -> EndpointStarter {
    Arc::new(|instances, persistence, id, generation, endpoint_info| {
        Box::pin(start_realm_endpoint(
            instances,
            persistence,
            id,
            generation,
            endpoint_info,
        ))
    })
}

#[derive(Default)]
pub struct InstanceStats {
    total_inbound_bytes: AtomicU64,
    total_outbound_bytes: AtomicU64,
    total_connections: AtomicU64,
    tcp_inbound_bytes: AtomicU64,
    tcp_outbound_bytes: AtomicU64,
    tcp_total_connections: AtomicU64,
    udp_inbound_bytes: AtomicU64,
    udp_outbound_bytes: AtomicU64,
    udp_total_connections: AtomicU64,
    next_conn_id: AtomicU64,
    connections: std::sync::Mutex<HashMap<u64, ConnectionEntry>>,
    tcp_bytes_by_backend: std::sync::Mutex<HashMap<String, BackendBytes>>,
    udp_sessions: std::sync::Mutex<HashMap<SocketAddr, UdpSessionEntry>>,
    last_success_backend: std::sync::Mutex<Option<String>>,
    #[cfg(feature = "balance")]
    failover_health: std::sync::Mutex<Option<std::sync::Arc<realm_core::tcp::health::FailoverHealth>>>,
}

struct ConnectionEntry {
    peer: SocketAddr,
    started_at: Instant,
    backend: Option<String>,
    inbound_bytes: u64,
    outbound_bytes: u64,
}

struct UdpSessionEntry {
    peer: SocketAddr,
    started_at: Instant,
}

impl InstanceStats {
    fn clear_runtime_state(&self) {
        {
            let mut conns = match self.connections.lock() {
                Ok(x) => x,
                Err(e) => e.into_inner(),
            };
            conns.clear();
        }
        {
            let mut sessions = match self.udp_sessions.lock() {
                Ok(x) => x,
                Err(e) => e.into_inner(),
            };
            sessions.clear();
        }
        {
            let mut last = match self.last_success_backend.lock() {
                Ok(x) => x,
                Err(e) => e.into_inner(),
            };
            *last = None;
        }
        #[cfg(feature = "balance")]
        {
            let mut h = match self.failover_health.lock() {
                Ok(x) => x,
                Err(e) => e.into_inner(),
            };
            *h = None;
        }
    }

    fn get_last_success_backend(&self) -> Option<String> {
        match self.last_success_backend.lock() {
            Ok(x) => x.clone(),
            Err(e) => e.into_inner().clone(),
        }
    }

    #[cfg(feature = "balance")]
    fn get_failover_health(&self) -> Option<std::sync::Arc<realm_core::tcp::health::FailoverHealth>> {
        match self.failover_health.lock() {
            Ok(x) => x.clone(),
            Err(e) => e.into_inner().clone(),
        }
    }
}

impl TcpObserver for InstanceStats {
    fn on_connection_open(&self, peer: SocketAddr) -> u64 {
        let id = self.next_conn_id.fetch_add(1, Ordering::Relaxed).saturating_add(1);
        self.total_connections.fetch_add(1, Ordering::Relaxed);
        self.tcp_total_connections.fetch_add(1, Ordering::Relaxed);
        let mut conns = match self.connections.lock() {
            Ok(x) => x,
            Err(e) => e.into_inner(),
        };
        conns.insert(
            id,
            ConnectionEntry {
                peer,
                started_at: Instant::now(),
                backend: None,
                inbound_bytes: 0,
                outbound_bytes: 0,
            },
        );
        id
    }

    fn on_connection_backend(&self, id: u64, backend: &realm_core::endpoint::RemoteAddr) {
        let backend = backend.to_string();
        {
            let mut conns = match self.connections.lock() {
                Ok(x) => x,
                Err(e) => e.into_inner(),
            };
            if let Some(entry) = conns.get_mut(&id) {
                entry.backend = Some(backend.clone());
            }
        }
        {
            let mut last = match self.last_success_backend.lock() {
                Ok(x) => x,
                Err(e) => e.into_inner(),
            };
            *last = Some(backend);
        }
    }

    fn on_connection_bytes(&self, id: u64, inbound_delta: u64, outbound_delta: u64) {
        if inbound_delta > 0 {
            self.total_inbound_bytes.fetch_add(inbound_delta, Ordering::Relaxed);
            self.tcp_inbound_bytes.fetch_add(inbound_delta, Ordering::Relaxed);
        }
        if outbound_delta > 0 {
            self.total_outbound_bytes.fetch_add(outbound_delta, Ordering::Relaxed);
            self.tcp_outbound_bytes.fetch_add(outbound_delta, Ordering::Relaxed);
        }

        let backend = if inbound_delta > 0 || outbound_delta > 0 {
            let mut conns = match self.connections.lock() {
                Ok(x) => x,
                Err(e) => e.into_inner(),
            };
            if let Some(entry) = conns.get_mut(&id) {
                entry.inbound_bytes = entry.inbound_bytes.saturating_add(inbound_delta);
                entry.outbound_bytes = entry.outbound_bytes.saturating_add(outbound_delta);
                entry.backend.clone()
            } else {
                None
            }
        } else {
            None
        };

        let Some(backend) = backend else {
            return;
        };

        let mut agg = match self.tcp_bytes_by_backend.lock() {
            Ok(x) => x,
            Err(e) => e.into_inner(),
        };
        let bb = agg.entry(backend).or_default();
        bb.inbound_bytes = bb.inbound_bytes.saturating_add(inbound_delta);
        bb.outbound_bytes = bb.outbound_bytes.saturating_add(outbound_delta);
    }

    fn on_connection_end(&self, id: u64, _error: Option<String>) {
        let mut conns = match self.connections.lock() {
            Ok(x) => x,
            Err(e) => e.into_inner(),
        };
        conns.remove(&id);
    }

    #[cfg(feature = "balance")]
    fn on_failover_health(&self, health: Option<std::sync::Arc<realm_core::tcp::health::FailoverHealth>>) {
        let mut h = match self.failover_health.lock() {
            Ok(x) => x,
            Err(e) => e.into_inner(),
        };
        *h = health;
    }
}

impl UdpObserver for InstanceStats {
    fn on_session_open(&self, peer: SocketAddr) {
        self.total_connections.fetch_add(1, Ordering::Relaxed);
        self.udp_total_connections.fetch_add(1, Ordering::Relaxed);
        let mut sessions = match self.udp_sessions.lock() {
            Ok(x) => x,
            Err(e) => e.into_inner(),
        };
        sessions.insert(
            peer,
            UdpSessionEntry {
                peer,
                started_at: Instant::now(),
            },
        );
    }

    fn on_session_close(&self, peer: SocketAddr) {
        let mut sessions = match self.udp_sessions.lock() {
            Ok(x) => x,
            Err(e) => e.into_inner(),
        };
        sessions.remove(&peer);
    }

    fn on_bytes(&self, inbound_delta: u64, outbound_delta: u64) {
        if inbound_delta > 0 {
            self.total_inbound_bytes.fetch_add(inbound_delta, Ordering::Relaxed);
            self.udp_inbound_bytes.fetch_add(inbound_delta, Ordering::Relaxed);
        }
        if outbound_delta > 0 {
            self.total_outbound_bytes.fetch_add(outbound_delta, Ordering::Relaxed);
            self.udp_outbound_bytes.fetch_add(outbound_delta, Ordering::Relaxed);
        }
    }
}

#[derive(Serialize, Deserialize)]
pub struct InstanceStatsResponse {
    pub id: String,
    pub total_inbound_bytes: u64,
    pub total_outbound_bytes: u64,
    pub total_connections: u64,
    pub current_connections: u64,
    pub tcp_inbound_bytes: u64,
    pub tcp_outbound_bytes: u64,
    pub tcp_total_connections: u64,
    pub tcp_current_connections: u64,
    pub udp_inbound_bytes: u64,
    pub udp_outbound_bytes: u64,
    pub udp_total_sessions: u64,
    pub udp_current_sessions: u64,
    // Deprecated aliases kept for backward compatibility.
    pub udp_total_connections: u64,
    pub udp_current_connections: u64,
    #[serde(default)]
    pub connections_by_backend: HashMap<String, u64>,
    #[serde(default)]
    pub bytes_by_backend: HashMap<String, BackendBytes>,
}

#[derive(Serialize, Deserialize, Default, Clone, Debug, PartialEq, Eq)]
pub struct BackendBytes {
    pub inbound_bytes: u64,
    pub outbound_bytes: u64,
}

#[derive(Serialize, Deserialize)]
pub struct InstanceRouteBackend {
    pub addr: String,
    pub role: String,
    pub state: String,
    pub backoff_until_ms: Option<u64>,
    pub ok_recent: bool,
}

#[derive(Serialize, Deserialize)]
pub struct InstanceRouteResponse {
    pub id: String,
    pub strategy: String,
    pub preferred_backend: Option<String>,
    pub last_success_backend: Option<String>,
    pub backends: Vec<InstanceRouteBackend>,
    #[serde(default)]
    pub connections_by_backend: HashMap<String, u64>,
    #[serde(default)]
    pub bytes_by_backend: HashMap<String, BackendBytes>,
    pub updated_at: String,
}

#[derive(Serialize, Deserialize)]
pub struct ConnectionStats {
    pub src_ip: String,
    pub src_port: u16,
    pub duration_secs: u64,
    pub backend: String,
}

#[derive(Deserialize)]
pub struct ConnectionsQuery {
    #[serde(default)]
    pub protocol: Option<String>,
    #[serde(default)]
    pub limit: Option<usize>,
    #[serde(default)]
    pub offset: Option<usize>,
}

#[derive(Serialize, Deserialize)]
pub struct TcpConnectionsPageResponse {
    pub id: String,
    pub protocol: String,
    pub total: u64,
    pub limit: u64,
    pub offset: u64,
    pub connections: Vec<ConnectionStats>,
}

#[derive(Serialize, Deserialize)]
pub struct UdpSessionsPageResponse {
    pub id: String,
    pub protocol: String,
    pub total: u64,
    pub limit: u64,
    pub offset: u64,
    pub sessions: Vec<ConnectionStats>,
}

#[derive(Serialize, Deserialize)]
pub struct ConnectionsAndSessionsPageResponse {
    pub id: String,
    pub protocol: String,
    pub tcp_total: u64,
    pub udp_total: u64,
    pub limit: u64,
    pub offset: u64,
    pub connections: Vec<ConnectionStats>,
    pub sessions: Vec<ConnectionStats>,
}

#[derive(Serialize, Deserialize)]
#[serde(untagged)]
pub enum ConnectionsPageResponse {
    Tcp(TcpConnectionsPageResponse),
    Udp(UdpSessionsPageResponse),
    All(ConnectionsAndSessionsPageResponse),
}

pub struct InstanceData {
    pub instance: Instance,
    pub tcp_abort: Option<AbortHandle>,
    pub udp_abort: Option<AbortHandle>,
    pub generation: u64,
    pub created_at: String,
    pub updated_at: Option<String>,
    pub stats: Arc<InstanceStats>,
}

#[derive(Deserialize)]
pub struct CreateInstanceRequest {
    #[serde(default)]
    pub id: Option<String>,
    #[serde(default)]
    pub external_id: Option<String>,
    #[serde(flatten)]
    pub config: EndpointConf,
}

fn validate_instance_id(id: &str) -> Result<(), String> {
    let id = id.trim();
    if id.is_empty() {
        return Err("id must not be empty".to_string());
    }
    if id.len() > 256 {
        return Err("id too long (max 256)".to_string());
    }
    if id.chars().any(|c| c.is_whitespace()) {
        return Err("id must not contain whitespace".to_string());
    }
    if id.contains('/') || id.contains('\\') {
        return Err("id must not contain path separators".to_string());
    }
    Ok(())
}

async fn list_instances(State(state): State<AppState>) -> Json<Vec<Instance>> {
    let instances = state.instances.lock().await;
    let list: Vec<Instance> = instances.values().map(|data| data.instance.clone()).collect();
    Json(list)
}

async fn create_instance(
    State(state): State<AppState>,
    Json(req): Json<CreateInstanceRequest>,
) -> ApiResult<(StatusCode, Json<Instance>)> {
    if req.id.is_some() && req.external_id.is_some() && req.id != req.external_id {
        return Err((
            StatusCode::BAD_REQUEST,
            api_error("invalid_id", "id and external_id must match when both are provided"),
        ));
    }
    let mut config = req.config;

    if let Some(global_config) = &state.global_config {
        config.network.take_field(&global_config.network);
    }

    let endpoint_info = config
        .clone()
        .try_build()
        .map_err(|e| (StatusCode::BAD_REQUEST, api_error("invalid_config", e.to_string())))?;

    let id = match req.id.or(req.external_id) {
        Some(id) => {
            validate_instance_id(&id).map_err(|e| (StatusCode::BAD_REQUEST, api_error("invalid_id", e)))?;
            id
        }
        None => uuid::Uuid::new_v4().to_string(),
    };

    let (generation, status_code, persistence_needed) = {
        let mut instances = state.instances.lock().await;
        if let Some(data) = instances.get_mut(&id) {
            if let Some(tcp) = data.tcp_abort.take() {
                tcp.abort();
            }
            if let Some(udp) = data.udp_abort.take() {
                udp.abort();
            }
            data.stats.clear_runtime_state();
            data.generation = data.generation.saturating_add(1);
            data.instance.config = config.clone();
            data.instance.status = InstanceStatus::Stopped;
            data.updated_at = Some(now_rfc3339());
            (data.generation, StatusCode::OK, state.persistence.clone())
        } else {
            let instance = Instance {
                id: id.clone(),
                config: config.clone(),
                status: InstanceStatus::Stopped,
                auto_start: true,
            };
            instances.insert(
                id.clone(),
                InstanceData {
                    instance,
                    tcp_abort: None,
                    udp_abort: None,
                    generation: 1,
                    created_at: now_rfc3339(),
                    updated_at: None,
                    stats: Arc::new(InstanceStats::default()),
                },
            );
            (1, StatusCode::CREATED, state.persistence.clone())
        }
    };

    let start_result = (state.endpoint_starter)(
        state.instances.clone(),
        state.persistence.clone(),
        id.clone(),
        generation,
        endpoint_info,
    )
    .await;

    let mut instances = state.instances.lock().await;
    let Some(data) = instances.get_mut(&id) else {
        return Err((
            StatusCode::INTERNAL_SERVER_ERROR,
            api_error("internal_error", "instance disappeared during creation"),
        ));
    };

    match start_result {
        Ok((tcp_abort, udp_abort)) => {
            if !matches!(data.instance.status, InstanceStatus::Failed(_)) {
                data.tcp_abort = tcp_abort;
                data.udp_abort = udp_abort;
                data.instance.status = InstanceStatus::Running;
            }
            data.updated_at = Some(now_rfc3339());
        }
        Err(msg) => {
            data.instance.status = InstanceStatus::Failed(msg);
            data.tcp_abort = None;
            data.udp_abort = None;
            data.updated_at = Some(now_rfc3339());
        }
    }

    let instance = data.instance.clone();

    if let Some(persistence) = &persistence_needed {
        let persistence_clone = persistence.clone();
        let instances_snapshot = PersistenceManager::create_instances_snapshot(&instances);
        tokio::spawn(async move {
            if let Err(e) = persistence_clone.save_instances(&instances_snapshot).await {
                eprintln!("Failed to save instances: {}", e);
            }
        });
    }

    Ok((status_code, Json(instance)))
}

async fn get_instance(State(state): State<AppState>, Path(id): Path<String>) -> ApiResult<Json<Instance>> {
    let instances = state.instances.lock().await;
    if let Some(data) = instances.get(&id) {
        Ok(Json(data.instance.clone()))
    } else {
        Err((StatusCode::NOT_FOUND, api_error("not_found", "instance not found")))
    }
}

async fn get_instance_stats(
    State(state): State<AppState>,
    Path(id): Path<String>,
) -> ApiResult<Json<InstanceStatsResponse>> {
    let instances = state.instances.lock().await;
    let Some(data) = instances.get(&id) else {
        return Err((StatusCode::NOT_FOUND, api_error("not_found", "instance not found")));
    };

    let stats = data.stats.clone();
    let default_backend = data.instance.config.remote.clone();
    let tcp_current = match stats.connections.lock() {
        Ok(x) => x.len() as u64,
        Err(e) => e.into_inner().len() as u64,
    };
    let udp_current = match stats.udp_sessions.lock() {
        Ok(x) => x.len() as u64,
        Err(e) => e.into_inner().len() as u64,
    };

    let (connections_by_backend, bytes_by_backend) = build_backend_aggregates(&stats, &default_backend);

    let resp = InstanceStatsResponse {
        id: id.clone(),
        total_inbound_bytes: stats.total_inbound_bytes.load(Ordering::Relaxed),
        total_outbound_bytes: stats.total_outbound_bytes.load(Ordering::Relaxed),
        total_connections: stats.total_connections.load(Ordering::Relaxed),
        current_connections: tcp_current + udp_current,
        tcp_inbound_bytes: stats.tcp_inbound_bytes.load(Ordering::Relaxed),
        tcp_outbound_bytes: stats.tcp_outbound_bytes.load(Ordering::Relaxed),
        tcp_total_connections: stats.tcp_total_connections.load(Ordering::Relaxed),
        tcp_current_connections: tcp_current,
        udp_inbound_bytes: stats.udp_inbound_bytes.load(Ordering::Relaxed),
        udp_outbound_bytes: stats.udp_outbound_bytes.load(Ordering::Relaxed),
        udp_total_sessions: stats.udp_total_connections.load(Ordering::Relaxed),
        udp_current_sessions: udp_current,
        udp_total_connections: stats.udp_total_connections.load(Ordering::Relaxed),
        udp_current_connections: udp_current,
        connections_by_backend,
        bytes_by_backend,
    };

    Ok(Json(resp))
}

async fn get_instance_route(
    State(state): State<AppState>,
    Path(id): Path<String>,
) -> ApiResult<Json<InstanceRouteResponse>> {
    let (config, stats) = {
        let instances = state.instances.lock().await;
        let Some(data) = instances.get(&id) else {
            return Err((StatusCode::NOT_FOUND, api_error("not_found", "instance not found")));
        };
        (data.instance.config.clone(), data.stats.clone())
    };

    let strategy = config
        .balance
        .as_deref()
        .unwrap_or("off")
        .split_once(':')
        .map(|(s, _)| s)
        .unwrap_or_else(|| config.balance.as_deref().unwrap_or("off"))
        .trim()
        .to_lowercase();

    let last_success_backend = stats.get_last_success_backend();

    let mut addrs: Vec<String> = Vec::with_capacity(1 + config.extra_remotes.len());
    addrs.push(config.remote.clone());
    addrs.extend(config.extra_remotes.iter().cloned());

    let (connections_by_backend, bytes_by_backend) = build_backend_aggregates(&stats, &config.remote);

    let mut backends: Vec<InstanceRouteBackend> = Vec::with_capacity(addrs.len());
    let mut preferred_backend: Option<String> = None;

    if strategy == "failover" {
        #[cfg(feature = "balance")]
        {
            if let Some(health) = stats.get_failover_health() {
                for (i, addr) in addrs.iter().enumerate() {
                    let idx = i as u8;
                    let snap = health.peer_snapshot(idx);
                    let role = if i == 0 { "primary" } else { "backup" };
                    let (state, backoff_until_ms, ok_recent) = match snap {
                        Some(s) if s.should_skip => ("backoff", Some(s.down_until_ms), s.ok_recent),
                        Some(s) if s.ok_recent => ("healthy", None, true),
                        Some(s) if s.fail_count > 0 => ("unhealthy", None, false),
                        Some(_) => ("unknown", None, false),
                        None => ("unknown", None, false),
                    };
                    if preferred_backend.is_none() {
                        if let Some(s) = snap {
                            if !s.should_skip {
                                preferred_backend = Some(addr.clone());
                            }
                        } else {
                            preferred_backend = Some(addr.clone());
                        }
                    }
                    backends.push(InstanceRouteBackend {
                        addr: addr.clone(),
                        role: role.to_string(),
                        state: state.to_string(),
                        backoff_until_ms,
                        ok_recent,
                    });
                }
                if preferred_backend.is_none() && !addrs.is_empty() {
                    preferred_backend = Some(addrs[0].clone());
                }
            } else if !addrs.is_empty() {
                preferred_backend = Some(addrs[0].clone());
                for (i, addr) in addrs.iter().enumerate() {
                    backends.push(InstanceRouteBackend {
                        addr: addr.clone(),
                        role: if i == 0 {
                            "primary".to_string()
                        } else {
                            "backup".to_string()
                        },
                        state: "unknown".to_string(),
                        backoff_until_ms: None,
                        ok_recent: false,
                    });
                }
            }
        }
        #[cfg(not(feature = "balance"))]
        {
            preferred_backend = addrs.get(0).cloned();
            for (i, addr) in addrs.iter().enumerate() {
                backends.push(InstanceRouteBackend {
                    addr: addr.clone(),
                    role: if i == 0 {
                        "primary".to_string()
                    } else {
                        "backup".to_string()
                    },
                    state: "unknown".to_string(),
                    backoff_until_ms: None,
                    ok_recent: false,
                });
            }
        }
    } else {
        preferred_backend = addrs.get(0).cloned();
        for (i, addr) in addrs.iter().enumerate() {
            backends.push(InstanceRouteBackend {
                addr: addr.clone(),
                role: if i == 0 {
                    "primary".to_string()
                } else {
                    "backup".to_string()
                },
                state: "unknown".to_string(),
                backoff_until_ms: None,
                ok_recent: false,
            });
        }
    }

    Ok(Json(InstanceRouteResponse {
        id,
        strategy,
        preferred_backend,
        last_success_backend,
        backends,
        connections_by_backend,
        bytes_by_backend,
        updated_at: now_rfc3339(),
    }))
}

async fn get_instance_connections(
    State(state): State<AppState>,
    Path(id): Path<String>,
    axum::extract::Query(query): axum::extract::Query<ConnectionsQuery>,
) -> ApiResult<Json<ConnectionsPageResponse>> {
    let limit = query.limit.unwrap_or(100).min(1000);
    let offset = query.offset.unwrap_or(0);

    let (stats, default_backend) = {
        let instances = state.instances.lock().await;
        let Some(data) = instances.get(&id) else {
            return Err((StatusCode::NOT_FOUND, api_error("not_found", "instance not found")));
        };
        (data.stats.clone(), data.instance.config.remote.clone())
    };

    let protocol = query.protocol.as_deref().map(|x| x.to_lowercase());
    match protocol.as_deref() {
        Some("tcp") => {
            let mut rows: Vec<ConnectionStats> = {
                let conns = match stats.connections.lock() {
                    Ok(x) => x,
                    Err(e) => e.into_inner(),
                };
                conns
                    .values()
                    .map(|entry| ConnectionStats {
                        src_ip: entry.peer.ip().to_string(),
                        src_port: entry.peer.port(),
                        duration_secs: entry.started_at.elapsed().as_secs(),
                        backend: entry.backend.clone().unwrap_or_else(|| default_backend.clone()),
                    })
                    .collect()
            };

            rows.sort_by(|a, b| b.duration_secs.cmp(&a.duration_secs));
            let total = rows.len() as u64;
            let page = rows.into_iter().skip(offset).take(limit).collect::<Vec<_>>();

            Ok(Json(ConnectionsPageResponse::Tcp(TcpConnectionsPageResponse {
                id,
                protocol: "tcp".to_string(),
                total,
                limit: limit as u64,
                offset: offset as u64,
                connections: page,
            })))
        }
        Some("udp") => {
            let mut rows: Vec<ConnectionStats> = {
                let sessions = match stats.udp_sessions.lock() {
                    Ok(x) => x,
                    Err(e) => e.into_inner(),
                };
                sessions
                    .values()
                    .map(|entry| ConnectionStats {
                        src_ip: entry.peer.ip().to_string(),
                        src_port: entry.peer.port(),
                        duration_secs: entry.started_at.elapsed().as_secs(),
                        backend: default_backend.clone(),
                    })
                    .collect()
            };

            rows.sort_by(|a, b| b.duration_secs.cmp(&a.duration_secs));
            let total = rows.len() as u64;
            let page = rows.into_iter().skip(offset).take(limit).collect::<Vec<_>>();

            Ok(Json(ConnectionsPageResponse::Udp(UdpSessionsPageResponse {
                id,
                protocol: "udp".to_string(),
                total,
                limit: limit as u64,
                offset: offset as u64,
                sessions: page,
            })))
        }
        None => {
            let (mut tcp_rows, mut udp_rows): (Vec<ConnectionStats>, Vec<ConnectionStats>) = {
                let conns = match stats.connections.lock() {
                    Ok(x) => x,
                    Err(e) => e.into_inner(),
                };
                let sessions = match stats.udp_sessions.lock() {
                    Ok(x) => x,
                    Err(e) => e.into_inner(),
                };

                let tcp = conns
                    .values()
                    .map(|entry| ConnectionStats {
                        src_ip: entry.peer.ip().to_string(),
                        src_port: entry.peer.port(),
                        duration_secs: entry.started_at.elapsed().as_secs(),
                        backend: entry.backend.clone().unwrap_or_else(|| default_backend.clone()),
                    })
                    .collect::<Vec<_>>();
                let udp = sessions
                    .values()
                    .map(|entry| ConnectionStats {
                        src_ip: entry.peer.ip().to_string(),
                        src_port: entry.peer.port(),
                        duration_secs: entry.started_at.elapsed().as_secs(),
                        backend: default_backend.clone(),
                    })
                    .collect::<Vec<_>>();
                (tcp, udp)
            };

            tcp_rows.sort_by(|a, b| b.duration_secs.cmp(&a.duration_secs));
            udp_rows.sort_by(|a, b| b.duration_secs.cmp(&a.duration_secs));

            let tcp_total = tcp_rows.len() as u64;
            let udp_total = udp_rows.len() as u64;

            let connections = tcp_rows.into_iter().skip(offset).take(limit).collect::<Vec<_>>();
            let sessions = udp_rows.into_iter().skip(offset).take(limit).collect::<Vec<_>>();

            Ok(Json(ConnectionsPageResponse::All(ConnectionsAndSessionsPageResponse {
                id,
                protocol: "all".to_string(),
                tcp_total,
                udp_total,
                limit: limit as u64,
                offset: offset as u64,
                connections,
                sessions,
            })))
        }
        _ => Err((
            StatusCode::BAD_REQUEST,
            api_error("invalid_query", "protocol must be `tcp` or `udp`"),
        )),
    }
}

async fn update_instance(
    State(state): State<AppState>,
    Path(id): Path<String>,
    Json(mut config): Json<EndpointConf>,
) -> ApiResult<Json<Instance>> {
    if let Some(global_config) = &state.global_config {
        config.network.take_field(&global_config.network);
    }

    let endpoint_info = config
        .clone()
        .try_build()
        .map_err(|e| (StatusCode::BAD_REQUEST, api_error("invalid_config", e.to_string())))?;

    let (generation, persistence_needed) = {
        let mut instances = state.instances.lock().await;
        let Some(data) = instances.get_mut(&id) else {
            return Err((StatusCode::NOT_FOUND, api_error("not_found", "instance not found")));
        };

        if let Some(tcp) = data.tcp_abort.take() {
            tcp.abort();
        }
        if let Some(udp) = data.udp_abort.take() {
            udp.abort();
        }
        data.stats.clear_runtime_state();

        data.generation = data.generation.saturating_add(1);
        data.instance.config = config;
        data.instance.status = InstanceStatus::Stopped;
        data.updated_at = Some(now_rfc3339());

        (data.generation, state.persistence.clone())
    };

    let start_result = (state.endpoint_starter)(
        state.instances.clone(),
        state.persistence.clone(),
        id.clone(),
        generation,
        endpoint_info,
    )
    .await;

    let mut instances = state.instances.lock().await;
    let Some(data) = instances.get_mut(&id) else {
        return Err((
            StatusCode::INTERNAL_SERVER_ERROR,
            api_error("internal_error", "instance disappeared during update"),
        ));
    };

    match start_result {
        Ok((tcp_abort, udp_abort)) => {
            if !matches!(data.instance.status, InstanceStatus::Failed(_)) {
                data.tcp_abort = tcp_abort;
                data.udp_abort = udp_abort;
                data.instance.status = InstanceStatus::Running;
            }
        }
        Err(msg) => {
            data.instance.status = InstanceStatus::Failed(msg);
            data.tcp_abort = None;
            data.udp_abort = None;
        }
    }

    data.updated_at = Some(now_rfc3339());
    let instance = data.instance.clone();

    if let Some(persistence) = &persistence_needed {
        let persistence_clone = persistence.clone();
        let instances_snapshot = PersistenceManager::create_instances_snapshot(&instances);
        tokio::spawn(async move {
            if let Err(e) = persistence_clone.save_instances(&instances_snapshot).await {
                eprintln!("Failed to save instances: {}", e);
            }
        });
    }

    Ok(Json(instance))
}

async fn patch_instance_auto_start(
    State(state): State<AppState>,
    Path(id): Path<String>,
    Json(update): Json<InstanceAutoStartUpdate>,
) -> ApiResult<Json<Instance>> {
    let mut instances = state.instances.lock().await;
    if let Some(data) = instances.get_mut(&id) {
        data.instance.auto_start = update.auto_start;
        data.updated_at = Some(now_rfc3339());
        let instance = data.instance.clone();

        if let Some(persistence) = &state.persistence {
            let persistence_clone = persistence.clone();
            let instances_snapshot = PersistenceManager::create_instances_snapshot(&instances);
            tokio::spawn(async move {
                if let Err(e) = persistence_clone.save_instances(&instances_snapshot).await {
                    eprintln!("Failed to save instances: {}", e);
                }
            });
        }

        Ok(Json(instance))
    } else {
        Err((StatusCode::NOT_FOUND, api_error("not_found", "instance not found")))
    }
}

async fn start_instance(State(state): State<AppState>, Path(id): Path<String>) -> ApiResult<Json<Instance>> {
    let (endpoint_info, generation) = {
        let mut instances = state.instances.lock().await;
        let Some(data) = instances.get_mut(&id) else {
            return Err((StatusCode::NOT_FOUND, api_error("not_found", "instance not found")));
        };

        if matches!(data.instance.status, InstanceStatus::Running)
            && (data.tcp_abort.is_some() || data.udp_abort.is_some())
        {
            return Err((StatusCode::CONFLICT, api_error("conflict", "instance already running")));
        }

        let mut config = data.instance.config.clone();
        if let Some(global_config) = &state.global_config {
            config.network.take_field(&global_config.network);
        }

        let endpoint_info = match config.try_build() {
            Ok(info) => info,
            Err(e) => {
                data.instance.status = InstanceStatus::Failed(e.to_string());
                data.updated_at = Some(now_rfc3339());
                if let Some(persistence) = &state.persistence {
                    let persistence_clone = persistence.clone();
                    let instances_snapshot = PersistenceManager::create_instances_snapshot(&instances);
                    tokio::spawn(async move {
                        if let Err(e) = persistence_clone.save_instances(&instances_snapshot).await {
                            eprintln!("Failed to save instances: {}", e);
                        }
                    });
                }
                return Err((StatusCode::BAD_REQUEST, api_error("invalid_config", e.to_string())));
            }
        };

        data.stats.clear_runtime_state();
        data.generation = data.generation.saturating_add(1);
        data.instance.status = InstanceStatus::Stopped;
        data.updated_at = Some(now_rfc3339());
        (endpoint_info, data.generation)
    };

    let start_result = (state.endpoint_starter)(
        state.instances.clone(),
        state.persistence.clone(),
        id.clone(),
        generation,
        endpoint_info,
    )
    .await;

    let mut instances = state.instances.lock().await;
    let Some(data) = instances.get_mut(&id) else {
        return Err((
            StatusCode::INTERNAL_SERVER_ERROR,
            api_error("internal_error", "instance disappeared during start"),
        ));
    };

    match start_result {
        Ok((tcp_abort, udp_abort)) => {
            if !matches!(data.instance.status, InstanceStatus::Failed(_)) {
                data.tcp_abort = tcp_abort;
                data.udp_abort = udp_abort;
                data.instance.status = InstanceStatus::Running;
            }
        }
        Err(msg) => {
            data.instance.status = InstanceStatus::Failed(msg);
            data.tcp_abort = None;
            data.udp_abort = None;
        }
    }

    data.updated_at = Some(now_rfc3339());
    let instance = data.instance.clone();

    if let Some(persistence) = &state.persistence {
        let persistence_clone = persistence.clone();
        let instances_snapshot = PersistenceManager::create_instances_snapshot(&instances);
        tokio::spawn(async move {
            if let Err(e) = persistence_clone.save_instances(&instances_snapshot).await {
                eprintln!("Failed to save instances: {}", e);
            }
        });
    }

    Ok(Json(instance))
}

async fn stop_instance(State(state): State<AppState>, Path(id): Path<String>) -> ApiResult<Json<Instance>> {
    let mut instances = state.instances.lock().await;
    let Some(data) = instances.get_mut(&id) else {
        return Err((StatusCode::NOT_FOUND, api_error("not_found", "instance not found")));
    };

    if data.tcp_abort.is_none() && data.udp_abort.is_none() && !matches!(data.instance.status, InstanceStatus::Running)
    {
        return Err((StatusCode::CONFLICT, api_error("conflict", "instance already stopped")));
    }

    if let Some(tcp) = data.tcp_abort.take() {
        tcp.abort();
    }
    if let Some(udp) = data.udp_abort.take() {
        udp.abort();
    }
    data.stats.clear_runtime_state();

    data.instance.status = InstanceStatus::Stopped;
    data.updated_at = Some(now_rfc3339());
    let instance = data.instance.clone();

    if let Some(persistence) = &state.persistence {
        let persistence_clone = persistence.clone();
        let instances_snapshot = PersistenceManager::create_instances_snapshot(&instances);
        tokio::spawn(async move {
            if let Err(e) = persistence_clone.save_instances(&instances_snapshot).await {
                eprintln!("Failed to save instances: {}", e);
            }
        });
    }

    Ok(Json(instance))
}

async fn restart_instance(State(state): State<AppState>, Path(id): Path<String>) -> ApiResult<Json<Instance>> {
    let (endpoint_info, generation) = {
        let mut instances = state.instances.lock().await;
        let Some(data) = instances.get_mut(&id) else {
            return Err((StatusCode::NOT_FOUND, api_error("not_found", "instance not found")));
        };

        if let Some(tcp) = data.tcp_abort.take() {
            tcp.abort();
        }
        if let Some(udp) = data.udp_abort.take() {
            udp.abort();
        }
        data.stats.clear_runtime_state();

        let mut config = data.instance.config.clone();
        if let Some(global_config) = &state.global_config {
            config.network.take_field(&global_config.network);
        }

        let endpoint_info = config
            .try_build()
            .map_err(|e| (StatusCode::BAD_REQUEST, api_error("invalid_config", e.to_string())))?;

        data.generation = data.generation.saturating_add(1);
        data.instance.status = InstanceStatus::Stopped;
        data.updated_at = Some(now_rfc3339());
        (endpoint_info, data.generation)
    };

    let start_result = (state.endpoint_starter)(
        state.instances.clone(),
        state.persistence.clone(),
        id.clone(),
        generation,
        endpoint_info,
    )
    .await;

    let mut instances = state.instances.lock().await;
    let Some(data) = instances.get_mut(&id) else {
        return Err((
            StatusCode::INTERNAL_SERVER_ERROR,
            api_error("internal_error", "instance disappeared during restart"),
        ));
    };

    match start_result {
        Ok((tcp_abort, udp_abort)) => {
            if !matches!(data.instance.status, InstanceStatus::Failed(_)) {
                data.tcp_abort = tcp_abort;
                data.udp_abort = udp_abort;
                data.instance.status = InstanceStatus::Running;
            }
        }
        Err(msg) => {
            data.instance.status = InstanceStatus::Failed(msg);
            data.tcp_abort = None;
            data.udp_abort = None;
        }
    }

    data.updated_at = Some(now_rfc3339());
    let instance = data.instance.clone();

    if let Some(persistence) = &state.persistence {
        let persistence_clone = persistence.clone();
        let instances_snapshot = PersistenceManager::create_instances_snapshot(&instances);
        tokio::spawn(async move {
            if let Err(e) = persistence_clone.save_instances(&instances_snapshot).await {
                eprintln!("Failed to save instances: {}", e);
            }
        });
    }

    Ok(Json(instance))
}

async fn delete_instance(State(state): State<AppState>, Path(id): Path<String>) -> ApiResult<StatusCode> {
    let mut instances = state.instances.lock().await;
    if let Some(data) = instances.remove(&id) {
        data.stats.clear_runtime_state();
        if let Some(tcp) = data.tcp_abort {
            tcp.abort();
        }
        if let Some(udp) = data.udp_abort {
            udp.abort();
        }

        if let Some(persistence) = &state.persistence {
            let persistence_clone = persistence.clone();
            let instances_snapshot = PersistenceManager::create_instances_snapshot(&instances);
            tokio::spawn(async move {
                if let Err(e) = persistence_clone.save_instances(&instances_snapshot).await {
                    eprintln!("Failed to save instances: {}", e);
                }
            });
        }

        Ok(StatusCode::NO_CONTENT)
    } else {
        Err((StatusCode::NOT_FOUND, api_error("not_found", "instance not found")))
    }
}

async fn start_realm_endpoint(
    instances: Arc<AsyncMutex<HashMap<String, InstanceData>>>,
    persistence: Option<PersistenceManager>,
    id: String,
    generation: u64,
    endpoint_info: EndpointInfo,
) -> Result<(Option<AbortHandle>, Option<AbortHandle>), String> {
    {
        let guard = instances.lock().await;
        let Some(data) = guard.get(&id) else {
            return Err("instance not found".to_string());
        };
        if data.generation != generation {
            return Err("instance generation changed during start".to_string());
        }
    }

    let EndpointInfo {
        endpoint,
        no_tcp,
        use_udp,
    } = endpoint_info;

    let mut tcp_abort = None;
    let mut udp_abort = None;
    let mut tcp_ready = None;
    let mut udp_ready = None;

    let tcp_observer: Option<Arc<dyn TcpObserver>> = {
        let guard = instances.lock().await;
        guard.get(&id).map(|data| {
            let o: Arc<dyn TcpObserver> = data.stats.clone();
            o
        })
    };
    let udp_observer: Option<Arc<dyn UdpObserver>> = {
        let guard = instances.lock().await;
        guard.get(&id).map(|data| {
            let o: Arc<dyn UdpObserver> = data.stats.clone();
            o
        })
    };

    if use_udp {
        let endpoint_clone = endpoint.clone();
        let (ready_tx, ready_rx) = oneshot::channel();
        let observer = udp_observer.clone();
        let join: JoinHandle<std::io::Result<()>> = tokio::spawn(async move {
            match observer {
                Some(obs) => realm_core::udp::run_udp_with_ready_and_observer(endpoint_clone, ready_tx, obs).await,
                None => realm_core::udp::run_udp_with_ready(endpoint_clone, ready_tx).await,
            }
        });
        let handle = join.abort_handle();
        {
            let mut guard = instances.lock().await;
            let Some(data) = guard.get_mut(&id) else {
                handle.abort();
                return Err("instance not found".to_string());
            };
            if data.generation != generation {
                handle.abort();
                return Err("instance generation changed during start".to_string());
            }
            data.udp_abort = Some(handle.clone());
        }
        udp_abort = Some(handle);
        udp_ready = Some(ready_rx);

        spawn_endpoint_watcher(
            instances.clone(),
            persistence.clone(),
            id.clone(),
            generation,
            "udp",
            join,
        );
    }

    if !no_tcp {
        let (ready_tx, ready_rx) = oneshot::channel();
        let observer = tcp_observer.clone();
        let join: JoinHandle<std::io::Result<()>> = tokio::spawn(async move {
            match observer {
                Some(obs) => realm_core::tcp::run_tcp_with_ready_and_observer(endpoint, ready_tx, obs).await,
                None => realm_core::tcp::run_tcp_with_ready(endpoint, ready_tx).await,
            }
        });
        let handle = join.abort_handle();
        {
            let mut guard = instances.lock().await;
            let Some(data) = guard.get_mut(&id) else {
                handle.abort();
                if let Some(udp) = udp_abort.take() {
                    udp.abort();
                }
                return Err("instance not found".to_string());
            };
            if data.generation != generation {
                handle.abort();
                if let Some(udp) = udp_abort.take() {
                    udp.abort();
                }
                return Err("instance generation changed during start".to_string());
            }
            data.tcp_abort = Some(handle.clone());
        }
        tcp_abort = Some(handle);
        tcp_ready = Some(ready_rx);

        spawn_endpoint_watcher(
            instances.clone(),
            persistence.clone(),
            id.clone(),
            generation,
            "tcp",
            join,
        );
    }

    if let Some(rx) = udp_ready {
        match timeout(Duration::from_secs(3), rx).await {
            Ok(Ok(Ok(()))) => {}
            Ok(Ok(Err(e))) => {
                if let Some(tcp) = tcp_abort.take() {
                    tcp.abort();
                }
                if let Some(udp) = udp_abort.take() {
                    udp.abort();
                }
                return Err(format!("udp bind failed: {}", e));
            }
            Ok(Err(_)) => {
                if let Some(tcp) = tcp_abort.take() {
                    tcp.abort();
                }
                if let Some(udp) = udp_abort.take() {
                    udp.abort();
                }
                return Err("udp startup failed (ready channel closed)".to_string());
            }
            Err(_) => {
                if let Some(tcp) = tcp_abort.take() {
                    tcp.abort();
                }
                if let Some(udp) = udp_abort.take() {
                    udp.abort();
                }
                return Err("udp startup timed out".to_string());
            }
        }
    }

    if let Some(rx) = tcp_ready {
        match timeout(Duration::from_secs(3), rx).await {
            Ok(Ok(Ok(()))) => {}
            Ok(Ok(Err(e))) => {
                if let Some(tcp) = tcp_abort.take() {
                    tcp.abort();
                }
                if let Some(udp) = udp_abort.take() {
                    udp.abort();
                }
                return Err(format!("tcp bind failed: {}", e));
            }
            Ok(Err(_)) => {
                if let Some(tcp) = tcp_abort.take() {
                    tcp.abort();
                }
                if let Some(udp) = udp_abort.take() {
                    udp.abort();
                }
                return Err("tcp startup failed (ready channel closed)".to_string());
            }
            Err(_) => {
                if let Some(tcp) = tcp_abort.take() {
                    tcp.abort();
                }
                if let Some(udp) = udp_abort.take() {
                    udp.abort();
                }
                return Err("tcp startup timed out".to_string());
            }
        }
    }

    Ok((tcp_abort, udp_abort))
}

fn spawn_endpoint_watcher(
    instances: Arc<AsyncMutex<HashMap<String, InstanceData>>>,
    persistence: Option<PersistenceManager>,
    id: String,
    generation: u64,
    protocol: &'static str,
    join: JoinHandle<std::io::Result<()>>,
) {
    tokio::spawn(async move {
        let exit = join.await;
        let msg = match exit {
            Ok(Ok(())) => format!("{} task exited", protocol),
            Ok(Err(e)) => format!("{} task error: {}", protocol, e),
            Err(e) if e.is_cancelled() => return,
            Err(e) if e.is_panic() => format!("{} task panicked", protocol),
            Err(e) => format!("{} task join error: {}", protocol, e),
        };

        let mut instances_guard = instances.lock().await;
        let Some(data) = instances_guard.get_mut(&id) else {
            return;
        };
        if data.generation != generation {
            return;
        }

        if protocol == "tcp" {
            data.tcp_abort = None;
            if let Some(udp) = data.udp_abort.take() {
                udp.abort();
            }
        } else {
            data.udp_abort = None;
            if let Some(tcp) = data.tcp_abort.take() {
                tcp.abort();
            }
        }

        data.instance.status = InstanceStatus::Failed(msg);
        data.updated_at = Some(now_rfc3339());

        if let Some(persistence) = &persistence {
            let persistence_clone = persistence.clone();
            let snapshot = PersistenceManager::create_instances_snapshot(&instances_guard);
            tokio::spawn(async move {
                if let Err(e) = persistence_clone.save_instances(&snapshot).await {
                    eprintln!("Failed to save instances: {}", e);
                }
            });
        }
    });
}

pub async fn start_api_server(
    bind: String,
    port: u16,
    api_key: Option<String>,
    global_config: Option<FullConf>,
    config_file: Option<String>,
) {
    let config = global_config.unwrap_or_else(|| {
        println!("No configuration file provided, using default global settings");
        FullConf::default()
    });

    let log_conf = config.log.clone();
    let (level, output) = log_conf.clone().build();
    fern::Dispatch::new()
        .format(|out, message, record| {
            out.finish(format_args!(
                "{}[{}][{}]{}",
                chrono::Local::now().format("[%Y-%m-%d][%H:%M:%S]"),
                record.target(),
                record.level(),
                message
            ))
        })
        .level(level)
        .chain(output)
        .apply()
        .unwrap_or_else(|e| eprintln!("Failed to setup logger: {}", e));
    println!("Global log configured: {}", log_conf);

    let dns_conf = config.dns.clone();
    let (conf, opts) = dns_conf.clone().build();
    realm_core::dns::build_lazy(conf, opts);
    println!("Global DNS configured: {}", dns_conf);

    #[cfg(feature = "transport")]
    {
        realm_core::kaminari::install_tls_provider();
    }

    let persistence = PersistenceManager::new(config_file, Some(config.clone()));

    let persisted_instances = match persistence.load_instances() {
        Ok(persisted_instances) => {
            println!("Loading {} saved instances...", persisted_instances.len());
            persisted_instances
        }
        Err(e) => {
            eprintln!("Failed to load instances: {}", e);
            vec![]
        }
    };

    let mut restored_instances = HashMap::new();
    for persisted in persisted_instances {
        let status = match persisted.status.as_str() {
            "Running" | "Stopped" => InstanceStatus::Stopped,
            s if s.starts_with("Failed(") => InstanceStatus::Failed(
                s.strip_prefix("Failed(")
                    .unwrap_or("Unknown error")
                    .strip_suffix(")")
                    .unwrap_or("Unknown error")
                    .to_string(),
            ),
            _ => InstanceStatus::Stopped,
        };

        let instance = Instance {
            id: persisted.id.clone(),
            config: persisted.config,
            status,
            auto_start: persisted.auto_start,
        };

        restored_instances.insert(
            persisted.id.clone(),
            InstanceData {
                instance,
                tcp_abort: None,
                udp_abort: None,
                generation: 0,
                created_at: persisted.created_at,
                updated_at: persisted.updated_at,
                stats: Arc::new(InstanceStats::default()),
            },
        );
    }

    let state = AppState {
        instances: Arc::new(AsyncMutex::new(restored_instances)),
        api_key: api_key.clone(),
        global_config: Some(config),
        persistence: Some(persistence),
        endpoint_starter: default_endpoint_starter(),
    };

    // Auto-start persisted instances.
    let auto_start_ids: Vec<String> = {
        let instances = state.instances.lock().await;
        instances
            .iter()
            .filter_map(|(id, data)| {
                if data.instance.auto_start && !matches!(data.instance.status, InstanceStatus::Failed(_)) {
                    Some(id.clone())
                } else {
                    None
                }
            })
            .collect()
    };

    for id in auto_start_ids {
        let (endpoint_info, generation) = {
            let mut instances = state.instances.lock().await;
            let Some(data) = instances.get_mut(&id) else {
                continue;
            };

            let mut config = data.instance.config.clone();
            if let Some(global_config) = &state.global_config {
                config.network.take_field(&global_config.network);
            }

            let endpoint_info = match config.try_build() {
                Ok(info) => info,
                Err(e) => {
                    data.instance.status = InstanceStatus::Failed(e.to_string());
                    data.updated_at = Some(now_rfc3339());
                    continue;
                }
            };

            data.generation = data.generation.saturating_add(1);
            data.updated_at = Some(now_rfc3339());
            (endpoint_info, data.generation)
        };

        let start_result = (state.endpoint_starter)(
            state.instances.clone(),
            state.persistence.clone(),
            id.clone(),
            generation,
            endpoint_info,
        )
        .await;

        let mut instances = state.instances.lock().await;
        if let Some(data) = instances.get_mut(&id) {
            match start_result {
                Ok((tcp_abort, udp_abort)) => {
                    if !matches!(data.instance.status, InstanceStatus::Failed(_)) {
                        data.tcp_abort = tcp_abort;
                        data.udp_abort = udp_abort;
                        data.instance.status = InstanceStatus::Running;
                        println!("Auto-started instance: {}", id);
                    } else {
                        eprintln!(
                            "Auto-start instance {} reported as failed during startup (task exited early)",
                            id
                        );
                    }
                }
                Err(msg) => {
                    let msg_copy = msg.clone();
                    data.instance.status = InstanceStatus::Failed(msg);
                    data.tcp_abort = None;
                    data.udp_abort = None;
                    eprintln!("Failed to auto-start instance {}: {}", id, msg_copy);
                }
            }
            data.updated_at = Some(now_rfc3339());

            if let Some(persistence) = &state.persistence {
                let persistence_clone = persistence.clone();
                let instances_snapshot = PersistenceManager::create_instances_snapshot(&instances);
                tokio::spawn(async move {
                    if let Err(e) = persistence_clone.save_instances(&instances_snapshot).await {
                        eprintln!("Failed to save instances: {}", e);
                    }
                });
            }
        }
    }

    let app = build_app(state);

    let addr = format!("{}:{}", bind, port);
    if let Some(_key) = &api_key {
        println!("Starting API server on {} with authentication enabled", addr);
        println!("API key loaded from REALM_API_KEY environment variable");
    } else {
        println!("Starting API server on {} without authentication", addr);
        println!("Set REALM_API_KEY environment variable to enable authentication");
    }
    let listener = match tokio::net::TcpListener::bind(&addr).await {
        Ok(listener) => listener,
        Err(e) => {
            eprintln!("Failed to bind API server on {}: {}", addr, e);
            return;
        }
    };

    if let Err(e) = axum::serve(listener, app).await {
        eprintln!("API server error: {}", e);
    }
}

fn build_app(state: AppState) -> Router {
    let api_routes = Router::new()
        .route("/instances", get(list_instances))
        .route("/instances", post(create_instance))
        .route("/instances/:id", get(get_instance))
        .route("/instances/:id/stats", get(get_instance_stats))
        .route("/instances/:id/route", get(get_instance_route))
        .route("/instances/:id/connections", get(get_instance_connections))
        .route("/instances/:id", put(update_instance))
        .route("/instances/:id", patch(patch_instance_auto_start))
        .route("/instances/:id", delete(delete_instance))
        .route("/instances/:id/start", post(start_instance))
        .route("/instances/:id/stop", post(stop_instance))
        .route("/instances/:id/restart", post(restart_instance))
        .layer(from_fn_with_state(state.clone(), auth_middleware));

    Router::new().merge(api_routes).with_state(state)
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::body::Body;
    use axum::extract::Query;
    use axum::http::Request;
    use http_body_util::BodyExt;
    use std::collections::HashMap as StdHashMap;
    use std::path::Path as StdPath;
    use tower::ServiceExt;

    fn ok_starter() -> EndpointStarter {
        Arc::new(|_instances, _persistence, _id, _generation, endpoint_info| {
            Box::pin(async move {
                let tcp = if !endpoint_info.no_tcp {
                    let join: JoinHandle<std::io::Result<()>> = tokio::spawn(async move {
                        tokio::time::sleep(Duration::from_secs(3600)).await;
                        Ok(())
                    });
                    Some(join.abort_handle())
                } else {
                    None
                };
                let udp = if endpoint_info.use_udp {
                    let join: JoinHandle<std::io::Result<()>> = tokio::spawn(async move {
                        tokio::time::sleep(Duration::from_secs(3600)).await;
                        Ok(())
                    });
                    Some(join.abort_handle())
                } else {
                    None
                };
                Ok((tcp, udp))
            })
        })
    }

    fn err_starter(msg: &'static str) -> EndpointStarter {
        Arc::new(move |_instances, _persistence, _id, _generation, _endpoint_info| {
            Box::pin(async move { Err(msg.to_string()) })
        })
    }

    fn make_state_with(api_key: Option<&str>, global_tcp_timeout: Option<usize>, starter: EndpointStarter) -> AppState {
        let mut global = FullConf::default();
        if let Some(v) = global_tcp_timeout {
            global.network.tcp_timeout = Some(v);
        }
        AppState {
            instances: Arc::new(AsyncMutex::new(HashMap::new())),
            api_key: api_key.map(|s| s.to_string()),
            global_config: Some(global),
            persistence: None,
            endpoint_starter: starter,
        }
    }

    async fn http(app: Router, req: Request<Body>) -> (StatusCode, String) {
        let resp = app.oneshot(req).await.expect("request failed");
        let status = resp.status();
        let body = resp
            .into_body()
            .collect()
            .await
            .expect("body collect failed")
            .to_bytes();
        (status, String::from_utf8_lossy(&body).to_string())
    }

    fn json_body(value: serde_json::Value) -> Body {
        Body::from(value.to_string())
    }

    fn make_state() -> AppState {
        AppState {
            instances: Arc::new(AsyncMutex::new(HashMap::new())),
            api_key: None,
            global_config: Some(FullConf::default()),
            persistence: None,
            endpoint_starter: ok_starter(),
        }
    }

    async fn insert_instance(state: &AppState, id: &str, stats: Arc<InstanceStats>) {
        let instance = Instance {
            id: id.to_string(),
            config: EndpointConf {
                listen: "127.0.0.1:12345".to_string(),
                remote: "example.com:80".to_string(),
                extra_remotes: vec![],
                balance: None,
                through: None,
                interface: None,
                listen_interface: None,
                listen_transport: None,
                remote_transport: None,
                network: Default::default(),
            },
            status: InstanceStatus::Running,
            auto_start: true,
        };

        let mut guard = state.instances.lock().await;
        guard.insert(
            id.to_string(),
            InstanceData {
                instance,
                tcp_abort: None,
                udp_abort: None,
                generation: 1,
                created_at: "2020-01-01T00:00:00Z".to_string(),
                updated_at: None,
                stats,
            },
        );
    }

    #[test]
    fn auth_check_works() {
        let mut headers = HeaderMap::new();
        assert!(is_request_authorized(None, &headers));
        assert!(!is_request_authorized(Some("k"), &headers));

        headers.insert("X-API-Key", "k".parse().unwrap());
        assert!(is_request_authorized(Some("k"), &headers));
        assert!(!is_request_authorized(Some("k2"), &headers));
    }

    #[test]
    fn auth_rejects_invalid_header_value() {
        use axum::http::HeaderValue;

        let mut headers = HeaderMap::new();
        headers.insert(&X_API_KEY, HeaderValue::from_bytes(b"\xff").unwrap());
        assert!(!is_request_authorized(Some("k"), &headers));
    }

    #[tokio::test]
    async fn stats_endpoint_returns_expected_fields() {
        let state = make_state();
        let stats = Arc::new(InstanceStats::default());

        stats.total_inbound_bytes.fetch_add(10, Ordering::Relaxed);
        stats.total_outbound_bytes.fetch_add(20, Ordering::Relaxed);
        stats.tcp_inbound_bytes.fetch_add(7, Ordering::Relaxed);
        stats.tcp_outbound_bytes.fetch_add(8, Ordering::Relaxed);
        stats.udp_inbound_bytes.fetch_add(3, Ordering::Relaxed);
        stats.udp_outbound_bytes.fetch_add(12, Ordering::Relaxed);
        stats.tcp_total_connections.fetch_add(2, Ordering::Relaxed);
        stats.udp_total_connections.fetch_add(4, Ordering::Relaxed);
        stats.total_connections.fetch_add(6, Ordering::Relaxed);

        {
            let mut conns = stats.connections.lock().unwrap_or_else(|e| e.into_inner());
            conns.insert(
                1,
                ConnectionEntry {
                    peer: "1.1.1.1:1111".parse().unwrap(),
                    started_at: Instant::now(),
                    backend: Some("example.com:80".to_string()),
                    inbound_bytes: 7,
                    outbound_bytes: 8,
                },
            );
        }
        {
            let mut bytes = stats.tcp_bytes_by_backend.lock().unwrap_or_else(|e| e.into_inner());
            bytes.insert(
                "example.com:80".to_string(),
                BackendBytes {
                    inbound_bytes: 7,
                    outbound_bytes: 8,
                },
            );
        }
        {
            let mut sessions = stats.udp_sessions.lock().unwrap_or_else(|e| e.into_inner());
            sessions.insert(
                "2.2.2.2:2222".parse().unwrap(),
                UdpSessionEntry {
                    peer: "2.2.2.2:2222".parse().unwrap(),
                    started_at: Instant::now(),
                },
            );
            sessions.insert(
                "3.3.3.3:3333".parse().unwrap(),
                UdpSessionEntry {
                    peer: "3.3.3.3:3333".parse().unwrap(),
                    started_at: Instant::now(),
                },
            );
        }

        insert_instance(&state, "i1", stats.clone()).await;

        let Json(resp) = match get_instance_stats(State(state), Path("i1".to_string())).await {
            Ok(x) => x,
            Err((status, body)) => panic!(
                "unexpected error: status={}, code={}, message={}",
                status, body.0.error.code, body.0.error.message
            ),
        };
        assert_eq!(resp.id, "i1");
        assert_eq!(resp.total_inbound_bytes, 10);
        assert_eq!(resp.total_outbound_bytes, 20);
        assert_eq!(resp.tcp_inbound_bytes, 7);
        assert_eq!(resp.tcp_outbound_bytes, 8);
        assert_eq!(resp.udp_inbound_bytes, 3);
        assert_eq!(resp.udp_outbound_bytes, 12);
        assert_eq!(resp.tcp_current_connections, 1);
        assert_eq!(resp.udp_current_sessions, 2);
        assert_eq!(resp.current_connections, 3);
        assert_eq!(resp.udp_total_sessions, 4);
        assert_eq!(resp.udp_total_connections, 4);
        assert_eq!(resp.udp_current_connections, 2);

        assert_eq!(resp.connections_by_backend.len(), 1);
        assert_eq!(resp.connections_by_backend.get("example.com:80").copied(), Some(3));
        assert_eq!(resp.bytes_by_backend.len(), 1);
        assert_eq!(
            resp.bytes_by_backend.get("example.com:80"),
            Some(&BackendBytes {
                inbound_bytes: 10,
                outbound_bytes: 20,
            })
        );
    }

    #[tokio::test]
    async fn stats_endpoint_returns_not_found() {
        let state = make_state();
        let err = get_instance_stats(State(state), Path("missing".to_string()))
            .await
            .err()
            .expect("expected 404");
        assert_eq!(err.0, StatusCode::NOT_FOUND);
        assert_eq!(err.1 .0.error.code, "not_found");
    }

    #[tokio::test]
    async fn connections_endpoint_paging_and_protocol_validation() {
        let state = make_state();
        let stats = Arc::new(InstanceStats::default());

        {
            let mut conns = stats.connections.lock().unwrap_or_else(|e| e.into_inner());
            conns.insert(
                1,
                ConnectionEntry {
                    peer: "10.0.0.1:1001".parse().unwrap(),
                    started_at: Instant::now() - std::time::Duration::from_secs(10),
                    backend: None,
                    inbound_bytes: 0,
                    outbound_bytes: 0,
                },
            );
            conns.insert(
                2,
                ConnectionEntry {
                    peer: "10.0.0.2:1002".parse().unwrap(),
                    started_at: Instant::now() - std::time::Duration::from_secs(20),
                    backend: None,
                    inbound_bytes: 0,
                    outbound_bytes: 0,
                },
            );
            conns.insert(
                3,
                ConnectionEntry {
                    peer: "10.0.0.3:1003".parse().unwrap(),
                    started_at: Instant::now() - std::time::Duration::from_secs(30),
                    backend: None,
                    inbound_bytes: 0,
                    outbound_bytes: 0,
                },
            );
        }

        insert_instance(&state, "i2", stats.clone()).await;

        let err = get_instance_connections(
            State(state.clone()),
            Path("i2".to_string()),
            Query(ConnectionsQuery {
                protocol: Some("bad".to_string()),
                limit: None,
                offset: None,
            }),
        )
        .await
        .err()
        .expect("expected error for invalid protocol");
        assert_eq!(err.0, StatusCode::BAD_REQUEST);
        assert_eq!(err.1 .0.error.code, "invalid_query");

        let Json(page) = match get_instance_connections(
            State(state),
            Path("i2".to_string()),
            Query(ConnectionsQuery {
                protocol: Some("tcp".to_string()),
                limit: Some(1),
                offset: Some(1),
            }),
        )
        .await
        {
            Ok(x) => x,
            Err((status, body)) => panic!(
                "unexpected error: status={}, code={}, message={}",
                status, body.0.error.code, body.0.error.message
            ),
        };
        let ConnectionsPageResponse::Tcp(page) = page else {
            panic!("expected tcp response");
        };
        assert_eq!(page.protocol, "tcp");
        assert_eq!(page.total, 3);
        assert_eq!(page.limit, 1);
        assert_eq!(page.offset, 1);
        assert_eq!(page.connections.len(), 1);
    }

    #[tokio::test]
    async fn connections_endpoint_defaults_to_tcp_and_udp() {
        let state = make_state();
        let stats = Arc::new(InstanceStats::default());
        insert_instance(&state, "i4", stats).await;

        let Json(page) = match get_instance_connections(
            State(state),
            Path("i4".to_string()),
            Query(ConnectionsQuery {
                protocol: None,
                limit: None,
                offset: None,
            }),
        )
        .await
        {
            Ok(x) => x,
            Err((status, body)) => panic!(
                "unexpected error: status={}, code={}, message={}",
                status, body.0.error.code, body.0.error.message
            ),
        };
        let ConnectionsPageResponse::All(page) = page else {
            panic!("expected all response");
        };
        assert_eq!(page.protocol, "all");
        assert_eq!(page.tcp_total, 0);
        assert_eq!(page.udp_total, 0);
    }

    #[tokio::test]
    async fn connections_endpoint_udp_uses_sessions_field() {
        let state = make_state();
        let stats = Arc::new(InstanceStats::default());
        {
            let mut sessions = stats.udp_sessions.lock().unwrap_or_else(|e| e.into_inner());
            sessions.insert(
                "10.0.0.9:9999".parse().unwrap(),
                UdpSessionEntry {
                    peer: "10.0.0.9:9999".parse().unwrap(),
                    started_at: Instant::now() - std::time::Duration::from_secs(5),
                },
            );
        }
        insert_instance(&state, "i_udp", stats).await;

        let Json(page) = get_instance_connections(
            State(state),
            Path("i_udp".to_string()),
            Query(ConnectionsQuery {
                protocol: Some("udp".to_string()),
                limit: Some(10),
                offset: Some(0),
            }),
        )
        .await
        .unwrap_or_else(|(status, body)| {
            panic!(
                "unexpected error: status={}, code={}, message={}",
                status, body.0.error.code, body.0.error.message
            )
        });

        let ConnectionsPageResponse::Udp(page) = page else {
            panic!("expected udp response");
        };
        assert_eq!(page.protocol, "udp");
        assert_eq!(page.total, 1);
        assert_eq!(page.sessions.len(), 1);
        assert_eq!(page.sessions[0].src_ip, "10.0.0.9");
    }

    #[tokio::test]
    async fn connections_endpoint_clamps_limit_and_handles_large_offset() {
        let state = make_state();
        let stats = Arc::new(InstanceStats::default());
        {
            let mut conns = stats.connections.lock().unwrap_or_else(|e| e.into_inner());
            conns.insert(
                1,
                ConnectionEntry {
                    peer: "10.0.0.1:1001".parse().unwrap(),
                    started_at: Instant::now() - std::time::Duration::from_secs(1),
                    backend: None,
                    inbound_bytes: 0,
                    outbound_bytes: 0,
                },
            );
        }
        insert_instance(&state, "i5", stats).await;

        let Json(page) = get_instance_connections(
            State(state.clone()),
            Path("i5".to_string()),
            Query(ConnectionsQuery {
                protocol: Some("tcp".to_string()),
                limit: Some(5000),
                offset: Some(0),
            }),
        )
        .await
        .unwrap_or_else(|(status, body)| {
            panic!(
                "unexpected error: status={}, code={}, message={}",
                status, body.0.error.code, body.0.error.message
            )
        });
        let ConnectionsPageResponse::Tcp(page) = page else {
            panic!("expected tcp response");
        };
        assert_eq!(page.limit, 1000);

        let Json(page2) = get_instance_connections(
            State(state),
            Path("i5".to_string()),
            Query(ConnectionsQuery {
                protocol: Some("tcp".to_string()),
                limit: Some(10),
                offset: Some(999),
            }),
        )
        .await
        .unwrap_or_else(|(status, body)| {
            panic!(
                "unexpected error: status={}, code={}, message={}",
                status, body.0.error.code, body.0.error.message
            )
        });
        let ConnectionsPageResponse::Tcp(page2) = page2 else {
            panic!("expected tcp response");
        };
        assert_eq!(page2.total, 1);
        assert!(page2.connections.is_empty());
    }

    #[tokio::test]
    async fn connections_endpoint_sorts_by_duration_desc() {
        let state = make_state();
        let stats = Arc::new(InstanceStats::default());
        {
            let mut conns = stats.connections.lock().unwrap_or_else(|e| e.into_inner());
            conns.insert(
                1,
                ConnectionEntry {
                    peer: "10.0.0.1:1001".parse().unwrap(),
                    started_at: Instant::now() - std::time::Duration::from_secs(10),
                    backend: None,
                    inbound_bytes: 0,
                    outbound_bytes: 0,
                },
            );
            conns.insert(
                2,
                ConnectionEntry {
                    peer: "10.0.0.2:1002".parse().unwrap(),
                    started_at: Instant::now() - std::time::Duration::from_secs(30),
                    backend: None,
                    inbound_bytes: 0,
                    outbound_bytes: 0,
                },
            );
        }
        insert_instance(&state, "i6", stats).await;

        let Json(page) = get_instance_connections(
            State(state),
            Path("i6".to_string()),
            Query(ConnectionsQuery {
                protocol: Some("tcp".to_string()),
                limit: Some(10),
                offset: Some(0),
            }),
        )
        .await
        .unwrap_or_else(|(status, body)| {
            panic!(
                "unexpected error: status={}, code={}, message={}",
                status, body.0.error.code, body.0.error.message
            )
        });

        let ConnectionsPageResponse::Tcp(page) = page else {
            panic!("expected tcp response");
        };
        assert_eq!(page.connections.len(), 2);
        assert!(page.connections[0].duration_secs >= page.connections[1].duration_secs);
        assert_eq!(page.connections[0].src_ip, "10.0.0.2");
    }

    #[tokio::test]
    async fn connections_endpoint_returns_not_found() {
        let state = make_state();
        let err = get_instance_connections(
            State(state),
            Path("missing".to_string()),
            Query(ConnectionsQuery {
                protocol: Some("tcp".to_string()),
                limit: None,
                offset: None,
            }),
        )
        .await
        .err()
        .expect("expected 404");
        assert_eq!(err.0, StatusCode::NOT_FOUND);
        assert_eq!(err.1 .0.error.code, "not_found");
    }

    #[tokio::test]
    async fn endpoint_watcher_marks_instance_failed_and_clears_handles() {
        let state = make_state();
        let stats = Arc::new(InstanceStats::default());
        insert_instance(&state, "i3", stats).await;

        let tcp_sleep: JoinHandle<std::io::Result<()>> = tokio::spawn(async move {
            tokio::time::sleep(Duration::from_secs(60)).await;
            Ok(())
        });
        let udp_sleep: JoinHandle<std::io::Result<()>> = tokio::spawn(async move {
            tokio::time::sleep(Duration::from_secs(60)).await;
            Ok(())
        });

        {
            let mut guard = state.instances.lock().await;
            let data = guard.get_mut("i3").unwrap();
            data.tcp_abort = Some(tcp_sleep.abort_handle());
            data.udp_abort = Some(udp_sleep.abort_handle());
            data.generation = 42;
            data.instance.status = InstanceStatus::Running;
        }

        let failing: JoinHandle<std::io::Result<()>> =
            tokio::spawn(async move { Err(std::io::Error::new(std::io::ErrorKind::Other, "boom")) });
        spawn_endpoint_watcher(state.instances.clone(), None, "i3".to_string(), 42, "tcp", failing);

        tokio::time::sleep(Duration::from_millis(50)).await;

        let guard = state.instances.lock().await;
        let data = guard.get("i3").unwrap();
        assert!(matches!(data.instance.status, InstanceStatus::Failed(_)));
        assert!(data.tcp_abort.is_none());
        assert!(data.udp_abort.is_none());
        assert!(data.updated_at.is_some());
    }

    #[tokio::test]
    async fn endpoint_watcher_ignores_generation_mismatch() {
        let state = make_state();
        let stats = Arc::new(InstanceStats::default());
        insert_instance(&state, "i7", stats).await;

        {
            let mut guard = state.instances.lock().await;
            let data = guard.get_mut("i7").unwrap();
            data.generation = 10;
            data.instance.status = InstanceStatus::Running;
        }

        let failing: JoinHandle<std::io::Result<()>> =
            tokio::spawn(async move { Err(std::io::Error::new(std::io::ErrorKind::Other, "boom")) });
        spawn_endpoint_watcher(state.instances.clone(), None, "i7".to_string(), 11, "tcp", failing);

        tokio::time::sleep(Duration::from_millis(50)).await;

        let guard = state.instances.lock().await;
        let data = guard.get("i7").unwrap();
        assert!(matches!(data.instance.status, InstanceStatus::Running));
    }

    #[tokio::test]
    async fn start_realm_endpoint_rejects_generation_mismatch_early() {
        use realm_core::endpoint::{BindOpts, ConnectOpts, Endpoint, RemoteAddr};

        let state = make_state();
        let stats = Arc::new(InstanceStats::default());
        insert_instance(&state, "i8", stats).await;
        {
            let mut guard = state.instances.lock().await;
            let data = guard.get_mut("i8").unwrap();
            data.generation = 1;
        }

        let endpoint = Endpoint {
            laddr: "127.0.0.1:0".parse().unwrap(),
            raddr: RemoteAddr::DomainName("example.com".to_string(), 80),
            bind_opts: BindOpts::default(),
            conn_opts: ConnectOpts::default(),
            extra_raddrs: vec![],
        };
        let info = EndpointInfo {
            no_tcp: true,
            use_udp: false,
            endpoint,
        };

        let err = start_realm_endpoint(state.instances.clone(), None, "i8".to_string(), 2, info)
            .await
            .unwrap_err();
        assert!(err.contains("generation"));
    }

    #[tokio::test]
    async fn persistence_manager_saves_toml_and_preserves_timestamps() {
        let base_dir = StdPath::new("target").join("test-artifacts");
        std::fs::create_dir_all(&base_dir).unwrap();
        let file_path = base_dir.join(format!("pm-{}.toml", uuid::Uuid::new_v4()));
        let file_path_str = file_path.to_string_lossy().to_string();

        let pm = PersistenceManager::new(Some(file_path_str.clone()), Some(FullConf::default()));

        let mut instances: StdHashMap<String, InstanceData> = StdHashMap::new();
        instances.insert(
            "x".to_string(),
            InstanceData {
                instance: Instance {
                    id: "x".to_string(),
                    config: EndpointConf {
                        listen: "127.0.0.1:1".to_string(),
                        remote: "example.com:80".to_string(),
                        extra_remotes: vec![],
                        balance: None,
                        through: None,
                        interface: None,
                        listen_interface: None,
                        listen_transport: None,
                        remote_transport: None,
                        network: Default::default(),
                    },
                    status: InstanceStatus::Failed("oops".to_string()),
                    auto_start: false,
                },
                tcp_abort: None,
                udp_abort: None,
                generation: 1,
                created_at: "2020-01-01T00:00:00Z".to_string(),
                updated_at: Some("2020-01-02T00:00:00Z".to_string()),
                stats: Arc::new(InstanceStats::default()),
            },
        );

        pm.save_instances(&instances).await.unwrap();

        let content = std::fs::read_to_string(&file_path).unwrap();
        let parsed = FullConf::from_conf_str(&content).unwrap();
        assert_eq!(parsed.instances.len(), 1);
        assert_eq!(parsed.instances[0].id, "x");
        assert_eq!(parsed.instances[0].created_at, "2020-01-01T00:00:00Z");
        assert_eq!(parsed.instances[0].updated_at.as_deref(), Some("2020-01-02T00:00:00Z"));
        assert!(parsed.instances[0].status.starts_with("Failed("));

        let tmp_path = format!("{}.tmp", file_path_str);
        assert!(!StdPath::new(&tmp_path).exists());

        let _ = std::fs::remove_file(&file_path);
    }

    #[tokio::test]
    async fn http_auth_is_enforced_when_api_key_set() {
        let state = make_state_with(Some("k"), None, ok_starter());
        let app = build_app(state.clone());

        let (status, body) = http(
            app.clone(),
            Request::builder()
                .method("GET")
                .uri("/instances")
                .body(Body::empty())
                .unwrap(),
        )
        .await;
        assert_eq!(status, StatusCode::UNAUTHORIZED);
        let v: serde_json::Value = serde_json::from_str(&body).unwrap();
        assert_eq!(v["error"]["code"], "unauthorized");

        let (status, _) = http(
            app.clone(),
            Request::builder()
                .method("GET")
                .uri("/instances")
                .header("X-API-Key", "bad")
                .body(Body::empty())
                .unwrap(),
        )
        .await;
        assert_eq!(status, StatusCode::UNAUTHORIZED);

        let (status, body) = http(
            app,
            Request::builder()
                .method("GET")
                .uri("/instances")
                .header("X-API-Key", "k")
                .body(Body::empty())
                .unwrap(),
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        let v: serde_json::Value = serde_json::from_str(&body).unwrap();
        assert!(v.is_array());
    }

    #[tokio::test]
    async fn http_crud_and_lifecycle_flow_matches_design() {
        let state = make_state_with(None, Some(5), ok_starter());
        let app = build_app(state.clone());

        // list empty
        let (status, body) = http(
            app.clone(),
            Request::builder()
                .method("GET")
                .uri("/instances")
                .body(Body::empty())
                .unwrap(),
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        let list: Vec<Instance> = serde_json::from_str(&body).unwrap();
        assert!(list.is_empty());

        // create
        let (status, body) = http(
            app.clone(),
            Request::builder()
                .method("POST")
                .uri("/instances")
                .header("Content-Type", "application/json")
                .body(json_body(serde_json::json!({
                    "listen": "127.0.0.1:0",
                    "remote": "example.com:80"
                })))
                .unwrap(),
        )
        .await;
        assert_eq!(status, StatusCode::CREATED);
        let created: Instance = serde_json::from_str(&body).unwrap();
        assert!(matches!(created.status, InstanceStatus::Running));
        assert_eq!(created.config.network.tcp_timeout, Some(5));

        // get
        let (status, body) = http(
            app.clone(),
            Request::builder()
                .method("GET")
                .uri(format!("/instances/{}", created.id))
                .body(Body::empty())
                .unwrap(),
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        let got: Instance = serde_json::from_str(&body).unwrap();
        assert_eq!(got.id, created.id);

        // stats & connections are reachable
        let (status, body) = http(
            app.clone(),
            Request::builder()
                .method("GET")
                .uri(format!("/instances/{}/stats", created.id))
                .body(Body::empty())
                .unwrap(),
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        let stats: InstanceStatsResponse = serde_json::from_str(&body).unwrap();
        assert_eq!(stats.id, created.id);

        let (status, body) = http(
            app.clone(),
            Request::builder()
                .method("GET")
                .uri(format!("/instances/{}/connections", created.id))
                .body(Body::empty())
                .unwrap(),
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        let conns: ConnectionsPageResponse = serde_json::from_str(&body).unwrap();
        match conns {
            ConnectionsPageResponse::All(conns) => {
                assert_eq!(conns.id, created.id);
                assert_eq!(conns.protocol, "all");
            }
            _ => panic!("expected all response"),
        }

        // patch auto_start
        let (status, body) = http(
            app.clone(),
            Request::builder()
                .method("PATCH")
                .uri(format!("/instances/{}", created.id))
                .header("Content-Type", "application/json")
                .body(json_body(serde_json::json!({ "auto_start": false })))
                .unwrap(),
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        let patched: Instance = serde_json::from_str(&body).unwrap();
        assert!(!patched.auto_start);

        // stop
        let (status, body) = http(
            app.clone(),
            Request::builder()
                .method("POST")
                .uri(format!("/instances/{}/stop", created.id))
                .body(Body::empty())
                .unwrap(),
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        let stopped: Instance = serde_json::from_str(&body).unwrap();
        assert!(matches!(stopped.status, InstanceStatus::Stopped));
        {
            let guard = state.instances.lock().await;
            let data = guard.get(&created.id).unwrap();
            assert!(data.tcp_abort.is_none());
            assert!(data.udp_abort.is_none());
        }

        // stop conflict
        let (status, body) = http(
            app.clone(),
            Request::builder()
                .method("POST")
                .uri(format!("/instances/{}/stop", created.id))
                .body(Body::empty())
                .unwrap(),
        )
        .await;
        assert_eq!(status, StatusCode::CONFLICT);
        let v: serde_json::Value = serde_json::from_str(&body).unwrap();
        assert_eq!(v["error"]["code"], "conflict");

        // start
        let (status, body) = http(
            app.clone(),
            Request::builder()
                .method("POST")
                .uri(format!("/instances/{}/start", created.id))
                .body(Body::empty())
                .unwrap(),
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        let started: Instance = serde_json::from_str(&body).unwrap();
        assert!(matches!(started.status, InstanceStatus::Running));
        {
            let guard = state.instances.lock().await;
            let data = guard.get(&created.id).unwrap();
            assert!(data.tcp_abort.is_some());
        }

        // start conflict
        let (status, _) = http(
            app.clone(),
            Request::builder()
                .method("POST")
                .uri(format!("/instances/{}/start", created.id))
                .body(Body::empty())
                .unwrap(),
        )
        .await;
        assert_eq!(status, StatusCode::CONFLICT);

        // update (PUT) should also inherit global defaults
        let (status, body) = http(
            app.clone(),
            Request::builder()
                .method("PUT")
                .uri(format!("/instances/{}", created.id))
                .header("Content-Type", "application/json")
                .body(json_body(serde_json::json!({
                    "listen": "127.0.0.1:0",
                    "remote": "example.com:81"
                })))
                .unwrap(),
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        let updated: Instance = serde_json::from_str(&body).unwrap();
        assert_eq!(updated.config.remote, "example.com:81");
        assert_eq!(updated.config.network.tcp_timeout, Some(5));

        // restart
        let before_gen = {
            let guard = state.instances.lock().await;
            guard.get(&created.id).unwrap().generation
        };
        let (status, body) = http(
            app.clone(),
            Request::builder()
                .method("POST")
                .uri(format!("/instances/{}/restart", created.id))
                .body(Body::empty())
                .unwrap(),
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        let restarted: Instance = serde_json::from_str(&body).unwrap();
        assert!(matches!(restarted.status, InstanceStatus::Running));
        let after_gen = {
            let guard = state.instances.lock().await;
            guard.get(&created.id).unwrap().generation
        };
        assert!(after_gen > before_gen);

        // delete
        let (status, body) = http(
            app.clone(),
            Request::builder()
                .method("DELETE")
                .uri(format!("/instances/{}", created.id))
                .body(Body::empty())
                .unwrap(),
        )
        .await;
        assert_eq!(status, StatusCode::NO_CONTENT);
        assert!(body.is_empty());

        // get after delete -> 404
        let (status, body) = http(
            app.clone(),
            Request::builder()
                .method("GET")
                .uri(format!("/instances/{}", created.id))
                .body(Body::empty())
                .unwrap(),
        )
        .await;
        assert_eq!(status, StatusCode::NOT_FOUND);
        let v: serde_json::Value = serde_json::from_str(&body).unwrap();
        assert_eq!(v["error"]["code"], "not_found");
    }

    #[tokio::test]
    async fn http_post_instances_supports_id_upsert() {
        let state = make_state_with(None, None, ok_starter());
        let app = build_app(state.clone());

        let (status, body) = http(
            app.clone(),
            Request::builder()
                .method("POST")
                .uri("/instances")
                .header("Content-Type", "application/json")
                .body(json_body(serde_json::json!({
                    "id": "fixed-id",
                    "listen": "127.0.0.1:0",
                    "remote": "example.com:80"
                })))
                .unwrap(),
        )
        .await;
        assert_eq!(status, StatusCode::CREATED);
        let created: Instance = serde_json::from_str(&body).unwrap();
        assert_eq!(created.id, "fixed-id");
        assert_eq!(created.config.remote, "example.com:80");

        let (status, body) = http(
            app.clone(),
            Request::builder()
                .method("POST")
                .uri("/instances")
                .header("Content-Type", "application/json")
                .body(json_body(serde_json::json!({
                    "id": "fixed-id",
                    "listen": "127.0.0.1:0",
                    "remote": "example.com:81"
                })))
                .unwrap(),
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        let updated: Instance = serde_json::from_str(&body).unwrap();
        assert_eq!(updated.id, "fixed-id");
        assert_eq!(updated.config.remote, "example.com:81");

        let guard = state.instances.lock().await;
        assert_eq!(guard.len(), 1);
        assert!(guard.contains_key("fixed-id"));
    }

    #[tokio::test]
    async fn http_route_endpoint_returns_preferred_and_last_success_backend() {
        let state = make_state_with(None, None, ok_starter());
        let app = build_app(state.clone());

        let stats = Arc::new(InstanceStats::default());

        #[cfg(feature = "balance")]
        {
            use realm_core::tcp::health::FailoverHealth;
            let health = Arc::new(FailoverHealth::new(2, 6000, 500, 30000));
            // force primary into backoff so preferred should switch to backup
            health.mark_fail(0);
            *stats.failover_health.lock().unwrap_or_else(|e| e.into_inner()) = Some(health);
        }
        *stats.last_success_backend.lock().unwrap_or_else(|e| e.into_inner()) = Some("2.2.2.2:443".to_string());

        {
            let mut guard = state.instances.lock().await;
            guard.insert(
                "i_route".to_string(),
                InstanceData {
                    instance: Instance {
                        id: "i_route".to_string(),
                        config: EndpointConf {
                            listen: "127.0.0.1:0".to_string(),
                            remote: "1.1.1.1:443".to_string(),
                            extra_remotes: vec!["2.2.2.2:443".to_string()],
                            balance: Some("failover".to_string()),
                            through: None,
                            interface: None,
                            listen_interface: None,
                            listen_transport: None,
                            remote_transport: None,
                            network: Default::default(),
                        },
                        status: InstanceStatus::Running,
                        auto_start: true,
                    },
                    tcp_abort: None,
                    udp_abort: None,
                    generation: 1,
                    created_at: now_rfc3339(),
                    updated_at: None,
                    stats,
                },
            );
        }

        let (status, body) = http(
            app,
            Request::builder()
                .method("GET")
                .uri("/instances/i_route/route")
                .body(Body::empty())
                .unwrap(),
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        let route: InstanceRouteResponse = serde_json::from_str(&body).unwrap();
        assert_eq!(route.id, "i_route");
        assert_eq!(route.strategy, "failover");
        assert_eq!(route.preferred_backend.as_deref(), Some("2.2.2.2:443"));
        assert_eq!(route.last_success_backend.as_deref(), Some("2.2.2.2:443"));
        assert_eq!(route.backends.len(), 2);
        assert_eq!(route.backends[0].addr, "1.1.1.1:443");
        assert_eq!(route.backends[0].role, "primary");
        assert_eq!(route.backends[1].addr, "2.2.2.2:443");
        assert_eq!(route.backends[1].role, "backup");

        // no live connections/sessions -> maps are empty (still present in JSON)
        assert!(route.connections_by_backend.is_empty());
        assert!(route.bytes_by_backend.is_empty());
    }

    #[tokio::test]
    async fn http_route_endpoint_returns_backend_aggregates() {
        let state = make_state_with(None, None, ok_starter());
        let app = build_app(state.clone());

        let stats = Arc::new(InstanceStats::default());
        {
            let mut conns = stats.connections.lock().unwrap_or_else(|e| e.into_inner());
            conns.insert(
                1,
                ConnectionEntry {
                    peer: "9.9.9.9:9999".parse().unwrap(),
                    started_at: Instant::now(),
                    backend: Some("1.1.1.1:443".to_string()),
                    inbound_bytes: 5,
                    outbound_bytes: 6,
                },
            );
            conns.insert(
                2,
                ConnectionEntry {
                    peer: "8.8.8.8:8888".parse().unwrap(),
                    started_at: Instant::now(),
                    backend: Some("2.2.2.2:443".to_string()),
                    inbound_bytes: 7,
                    outbound_bytes: 8,
                },
            );
        }
        {
            let mut bytes = stats.tcp_bytes_by_backend.lock().unwrap_or_else(|e| e.into_inner());
            bytes.insert(
                "1.1.1.1:443".to_string(),
                BackendBytes {
                    inbound_bytes: 5,
                    outbound_bytes: 6,
                },
            );
            bytes.insert(
                "2.2.2.2:443".to_string(),
                BackendBytes {
                    inbound_bytes: 7,
                    outbound_bytes: 8,
                },
            );
        }

        {
            let mut guard = state.instances.lock().await;
            guard.insert(
                "i_route2".to_string(),
                InstanceData {
                    instance: Instance {
                        id: "i_route2".to_string(),
                        config: EndpointConf {
                            listen: "127.0.0.1:0".to_string(),
                            remote: "1.1.1.1:443".to_string(),
                            extra_remotes: vec!["2.2.2.2:443".to_string()],
                            balance: Some("failover".to_string()),
                            through: None,
                            interface: None,
                            listen_interface: None,
                            listen_transport: None,
                            remote_transport: None,
                            network: Default::default(),
                        },
                        status: InstanceStatus::Running,
                        auto_start: true,
                    },
                    tcp_abort: None,
                    udp_abort: None,
                    generation: 1,
                    created_at: now_rfc3339(),
                    updated_at: None,
                    stats,
                },
            );
        }

        let (status, body) = http(
            app,
            Request::builder()
                .method("GET")
                .uri("/instances/i_route2/route")
                .body(Body::empty())
                .unwrap(),
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        let route: InstanceRouteResponse = serde_json::from_str(&body).unwrap();
        assert_eq!(route.id, "i_route2");
        assert_eq!(route.strategy, "failover");

        assert_eq!(route.connections_by_backend.get("1.1.1.1:443").copied(), Some(1));
        assert_eq!(route.connections_by_backend.get("2.2.2.2:443").copied(), Some(1));
        assert_eq!(route.connections_by_backend.len(), 2);

        assert_eq!(
            route.bytes_by_backend.get("1.1.1.1:443"),
            Some(&BackendBytes {
                inbound_bytes: 5,
                outbound_bytes: 6,
            })
        );
        assert_eq!(
            route.bytes_by_backend.get("2.2.2.2:443"),
            Some(&BackendBytes {
                inbound_bytes: 7,
                outbound_bytes: 8,
            })
        );
        assert_eq!(route.bytes_by_backend.len(), 2);
    }

    #[tokio::test]
    async fn http_create_invalid_config_returns_400() {
        let state = make_state_with(None, None, ok_starter());
        let app = build_app(state);

        let (status, body) = http(
            app,
            Request::builder()
                .method("POST")
                .uri("/instances")
                .header("Content-Type", "application/json")
                .body(json_body(serde_json::json!({
                    "listen": "bad",
                    "remote": "example.com:80"
                })))
                .unwrap(),
        )
        .await;
        assert_eq!(status, StatusCode::BAD_REQUEST);
        let v: serde_json::Value = serde_json::from_str(&body).unwrap();
        assert_eq!(v["error"]["code"], "invalid_config");
    }

    #[tokio::test]
    async fn http_start_failure_sets_failed_status() {
        let state = make_state_with(None, None, err_starter("boom"));
        let app = build_app(state.clone());

        let (status, body) = http(
            app.clone(),
            Request::builder()
                .method("POST")
                .uri("/instances")
                .header("Content-Type", "application/json")
                .body(json_body(serde_json::json!({
                    "listen": "127.0.0.1:0",
                    "remote": "example.com:80"
                })))
                .unwrap(),
        )
        .await;
        assert_eq!(status, StatusCode::CREATED);
        let created: Instance = serde_json::from_str(&body).unwrap();
        assert!(matches!(created.status, InstanceStatus::Failed(_)));

        // start endpoint should also return 200 but mark Failed(...)
        let (status, body) = http(
            app,
            Request::builder()
                .method("POST")
                .uri(format!("/instances/{}/start", created.id))
                .body(Body::empty())
                .unwrap(),
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        let started: Instance = serde_json::from_str(&body).unwrap();
        assert!(matches!(started.status, InstanceStatus::Failed(_)));
        {
            let guard = state.instances.lock().await;
            let data = guard.get(&created.id).unwrap();
            assert!(data.tcp_abort.is_none());
            assert!(data.udp_abort.is_none());
        }
    }
}
