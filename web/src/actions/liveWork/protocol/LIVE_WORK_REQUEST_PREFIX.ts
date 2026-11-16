/**
 * Tag of every get_session_live_work request.
 *
 * connectWS generic "error" handler recognizes the reply by this prefix and
 * swallows it silently instead of surfacing a banner (e.g. against an older
 * server without the handler, or on a transport hiccup): both must read as
 * "treat as empty", not as a user-visible failure.
 */
export const LIVE_WORK_REQUEST_PREFIX = "livework-";
