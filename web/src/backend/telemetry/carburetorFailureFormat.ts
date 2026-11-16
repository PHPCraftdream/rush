/** One isolated engine failure, in the shape the backend log_client_error expects. */
export interface CarburetorFailurePayload {
  message: string;
  source?: string;
  stack?: string;
}

/**
 * Wraps an isolated engine failure into a telemetry payload; the source keeps
 * its origin (`carburetor:delivery`, `carburetor:computed`, ...).
 *
 * @param error - the error the engine isolated
 * @param source - where it was isolated (delivery, transaction, settlement, update, computed, teardown)
 */
export const formatCarburetorFailure = (error: unknown, source: string): CarburetorFailurePayload => ({
  message: error instanceof Error ? error.message : String(error),
  source: `carburetor:${source}`,
  stack: error instanceof Error ? error.stack : undefined,
});
