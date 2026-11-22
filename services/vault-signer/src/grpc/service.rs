//! `signer.v1.VaultSignerService` implementation — a thin shim over
//! `engine::SignerCore` (held behind a single Mutex; all decision work is
//! synchronous and serialized by design — this vault's whole security model
//! assumes single-writer sequencing for slots/usage/audit).

use std::sync::{Arc, Mutex, MutexGuard};

use tonic::{Request, Response, Status};

use crate::engine::{AckOutcome, SignOutcome, SignerCore};
use super::signer_v1::vault_signer_service_server::VaultSignerService;
use super::signer_v1::*;

/// Shared handle — cloneable for `Server::add_service` and test harnesses.
#[derive(Clone)]
pub struct VaultSignerSvc {
    core: Arc<Mutex<SignerCore>>,
}

impl VaultSignerSvc {
    pub fn new(core: SignerCore) -> Self {
        Self {
            core: Arc::new(Mutex::new(core)),
        }
    }

    /// Shared handle to the core — needed by tests/ops tooling to inspect or
    /// mutate signer-local state (e.g. audit-tamper drills).
    pub fn core_handle(&self) -> Arc<Mutex<SignerCore>> {
        Arc::clone(&self.core)
    }

    fn with_core<R>(&self, f: impl FnOnce(&mut SignerCore) -> R) -> R {
        // A poisoned mutex means a previous handler panicked; rusqlite rolls
        // back uncommitted txns on drop so the DB is consistent — recover the
        // guard rather than DoS-ing the vault forever.
        let mut guard: MutexGuard<'_, SignerCore> =
            self.core.lock().unwrap_or_else(|e| e.into_inner());
        f(&mut guard)
    }
}

fn ack_response(o: AckOutcome) -> RegisterWithdrawalAddressResponse {
    RegisterWithdrawalAddressResponse {
        accepted: o.accepted,
        reject_code: o.reject_code,
        reason: o.reason,
        audit_seq: o.audit_seq,
    }
}

#[tonic::async_trait]
impl VaultSignerService for VaultSignerSvc {
    async fn register_withdrawal_address(
        &self,
        request: Request<RegisterWithdrawalAddressRequest>,
    ) -> Result<Response<RegisterWithdrawalAddressResponse>, Status> {
        let req = request.into_inner();
        let outcome = self.with_core(|c| c.register_withdrawal_address(&req));
        Ok(Response::new(ack_response(outcome)))
    }

    async fn revoke_withdrawal_address(
        &self,
        request: Request<RevokeWithdrawalAddressRequest>,
    ) -> Result<Response<RevokeWithdrawalAddressResponse>, Status> {
        let req = request.into_inner();
        let outcome = self.with_core(|c| c.revoke_withdrawal_address(&req));
        let o: AckOutcome = outcome;
        Ok(Response::new(RevokeWithdrawalAddressResponse {
            accepted: o.accepted,
            reject_code: o.reject_code,
            reason: o.reason,
            audit_seq: o.audit_seq,
        }))
    }

    async fn sign_transaction(
        &self,
        request: Request<SignTransactionRequest>,
    ) -> Result<Response<SignTransactionResponse>, Status> {
        let req = request.into_inner();
        let outcome = self.with_core(|c| c.sign_transaction(&req));
        let result = match outcome {
            SignOutcome::Signed {
                raw_tx,
                tx_hash,
                audit_seq,
            } => sign_transaction_response::Result::Signed(SignedTx {
                raw_tx,
                tx_hash: tx_hash.to_vec(),
                audit_seq,
            }),
            SignOutcome::Rejected {
                reject_code,
                reason,
                audit_seq,
            } => sign_transaction_response::Result::Rejected(Rejection {
                code: reject_code,
                reason,
                audit_seq,
            }),
        };
        Ok(Response::new(SignTransactionResponse {
            result: Some(result),
        }))
    }

    async fn verify_audit_chain(
        &self,
        request: Request<VerifyAuditChainRequest>,
    ) -> Result<Response<VerifyAuditChainResponse>, Status> {
        let req = request.into_inner();
        let v = self.with_core(|c| c.verify_audit_chain(req.from_seq, req.to_seq))
            .map_err(|e| Status::internal(format!("verify_audit_chain: {e}")))?;
        Ok(Response::new(VerifyAuditChainResponse {
            valid: v.valid,
            entries_checked: v.entries_checked,
            first_bad_seq: v.first_bad_seq,
            tip_hash: v.tip_hash.to_vec(),
        }))
    }

    async fn generate_wallet(
        &self,
        request: Request<GenerateWalletRequest>,
    ) -> Result<Response<GenerateWalletResponse>, Status> {
        let req = request.into_inner();
        let o = self.with_core(|c| c.generate_wallet(&req));
        // The response message carries no reject fields by contract — a
        // rejected call still lands a REJECTED entry on the audit chain
        // (server-side reason); the caller sees the cause via gRPC status
        // (INVALID_ARGUMENT for policy/input rejects, INTERNAL otherwise).
        if !o.accepted {
            return Err(if o.reject_code == RejectCode::Internal as i32 {
                Status::internal(o.reason)
            } else {
                Status::invalid_argument(o.reason)
            });
        }
        Ok(Response::new(GenerateWalletResponse {
            address: o.address,
            audit_seq: o.audit_seq,
        }))
    }

    async fn update_cold_wallets(
        &self,
        request: Request<UpdateColdWalletsRequest>,
    ) -> Result<Response<UpdateColdWalletsResponse>, Status> {
        let req = request.into_inner();
        let o = self.with_core(|c| c.update_cold_wallets(&req));
        Ok(Response::new(UpdateColdWalletsResponse {
            accepted: o.accepted,
            reject_code: o.reject_code,
            reason: o.reason,
            audit_seq: o.audit_seq,
            active_cold_wallets: o.active_cold_wallets,
        }))
    }

    async fn list_wallets(
        &self,
        _request: Request<ListWalletsRequest>,
    ) -> Result<Response<ListWalletsResponse>, Status> {
        let rows = self
            .with_core(|c| c.list_wallets())
            .map_err(|e| Status::internal(format!("list_wallets: {e}")))?;
        Ok(Response::new(ListWalletsResponse {
            wallets: rows
                .into_iter()
                .map(|w| HotWalletInfo {
                    address: w.address,
                    label: w.label,
                    created_at_unix: w.created_at_unix,
                    loaded: w.loaded,
                })
                .collect(),
        }))
    }

    async fn list_cold_wallets(
        &self,
        request: Request<ListColdWalletsRequest>,
    ) -> Result<Response<ListColdWalletsResponse>, Status> {
        let req = request.into_inner();
        let list = self
            .with_core(|c| c.list_cold_wallets(req.chain_id))
            .map_err(|e| Status::invalid_argument(e.to_string()))?;
        Ok(Response::new(ListColdWalletsResponse {
            active_cold_wallets: list,
        }))
    }

    async fn get_signer_health(
        &self,
        _request: Request<GetSignerHealthRequest>,
    ) -> Result<Response<GetSignerHealthResponse>, Status> {
        let h = self
            .with_core(|c| c.health())
            .map_err(|e| Status::internal(format!("health: {e}")))?;
        Ok(Response::new(GetSignerHealthResponse {
            version: h.version,
            uptime_seconds: h.uptime_seconds,
            signer_clock_unix: h.clock_unix,
            whitelist_entries: h.whitelist_entries,
            audit_entries: h.audit_entries,
            audit_tip_hash: h.audit_tip_hash.to_vec(),
            loaded_wallets: h.loaded_wallets,
        }))
    }
}
