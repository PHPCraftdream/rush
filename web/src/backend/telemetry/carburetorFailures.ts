// Forwards the failures the Carburetor engine isolates to the client telemetry.
//
// The engine catches instead of rethrowing (a throwing subscriber, computed,
// teardown, ...): without a handler such an error is only reported as text in
// development and silently dropped in production. The handler registered here
// runs in every build, in addition to the library's own development report.
import { diagnostics } from "@react-carburetor/core";
import { type CarburetorFailurePayload, formatCarburetorFailure } from "./carburetorFailureFormat.ts";

/**
 * Registers the single engine failure handler; every isolated failure goes to `report`.
 * Idempotent by contract: the engine keeps exactly one handler, re-registration replaces it.
 *
 * @param report - the sink for formatted payloads
 */
export function registerCarburetorFailureHandler(report: (payload: CarburetorFailurePayload) => void): void {
  diagnostics.setFailureHandler((error, source) => {
    report(formatCarburetorFailure(error, source));
  });
}
