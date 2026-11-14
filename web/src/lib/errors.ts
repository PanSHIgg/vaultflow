// Shared API error type — mirrors the openapi Error envelope.

import type { ErrorCode } from "./types";

export class ApiError extends Error {
  readonly status: number;
  readonly code: ErrorCode;
  readonly details?: Record<string, unknown>;

  constructor(status: number, code: ErrorCode, message: string, details?: Record<string, unknown>) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.details = details;
  }
}

export function apiError(
  status: number,
  code: ErrorCode,
  message: string,
  details?: Record<string, unknown>,
): ApiError {
  return new ApiError(status, code, message, details);
}
