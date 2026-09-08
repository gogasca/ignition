/** Error types raised by the Ignition SDK. */

export class IgnitionError extends Error {
  constructor(message: string) {
    super(message);
    this.name = new.target.name;
  }
}

export class APIError extends IgnitionError {
  readonly status: number;
  readonly code: string;
  readonly requestId: string;
  readonly retryable: boolean;
  readonly details: Record<string, unknown>;

  constructor(
    status: number,
    code = "",
    message = "",
    requestId = "",
    retryable = false,
    details: Record<string, unknown> = {},
  ) {
    super(`${code || `HTTP_${status}`}: ${message || status}${requestId ? ` (request ${requestId})` : ""}`);
    this.status = status;
    this.code = code || `HTTP_${status}`;
    this.requestId = requestId;
    this.retryable = retryable;
    this.details = details;
  }
}

export class NotFoundError extends APIError {}
export class PermissionDeniedError extends APIError {}
export class UnauthenticatedError extends APIError {}
export class ConflictError extends APIError {}
export class TimeoutError extends IgnitionError {}
export class StreamError extends IgnitionError {}

export function errorFromResponse(status: number, body: string, requestId: string): APIError {
  let code = "";
  let message = "";
  let retryable = false;
  let details: Record<string, unknown> = {};
  try {
    const parsed = JSON.parse(body || "{}");
    const err = parsed.error ?? parsed;
    code = err.code ?? "";
    message = err.message ?? "";
    retryable = Boolean(err.retryable);
    details = err.details ?? {};
    requestId = err.requestId ?? requestId;
  } catch {
    /* non-JSON error body */
  }
  const ctor =
    { 401: UnauthenticatedError, 403: PermissionDeniedError, 404: NotFoundError, 409: ConflictError }[
      status
    ] ?? APIError;
  return new ctor(status, code, message, requestId, retryable, details);
}
