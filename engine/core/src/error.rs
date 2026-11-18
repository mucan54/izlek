use thiserror::Error;

#[derive(Debug, Error)]
pub enum Error {
    #[error("object store: {0}")]
    Store(#[from] object_store::Error),
    #[error("json in {key}: {source}")]
    Json { key: String, source: serde_json::Error },
    #[error("parquet: {0}")]
    Parquet(#[from] parquet::errors::ParquetError),
    #[error("arrow: {0}")]
    Arrow(#[from] arrow::error::ArrowError),
    #[error("io: {0}")]
    Io(#[from] std::io::Error),
    #[error("config: {0}")]
    Config(String),
    #[error("invalid data: {0}")]
    Invalid(String),
    #[error("too many manifest conflicts on {0}")]
    Conflict(String),
}

pub type Result<T, E = Error> = std::result::Result<T, E>;

impl Error {
    pub fn invalid(msg: impl Into<String>) -> Self {
        Error::Invalid(msg.into())
    }
}
