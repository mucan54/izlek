use crate::exec::Step;
use thiserror::Error;

#[derive(Debug, Error)]
pub enum ShipError {
    #[error(transparent)]
    Core(#[from] la_core::Error),
    #[error("io: {0}")]
    Io(#[from] std::io::Error),
    /// A test hook asked for a crash right after this step.
    #[error("injected failure after {0:?}")]
    Injected(Step),
    #[error("{0}")]
    Other(String),
}

pub type Result<T, E = ShipError> = std::result::Result<T, E>;

impl ShipError {
    pub fn other(msg: impl Into<String>) -> Self {
        ShipError::Other(msg.into())
    }
}
