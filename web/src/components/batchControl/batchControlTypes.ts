/** Where a batch control request is addressed: a session, optionally narrowed to a root and node. */
export interface BatchControlTarget {
  sessionID: string;
  rootID?: string;
  nodeID?: string;
  waitMS?: number;
}

/** The server's verdict on a batch control request. */
export interface BatchControlResult {
  sessionID: string;
  rootID?: string;
  nodeID?: string;
  action: string;
  result: string;
  ok: boolean;
  outcomeSummary?: string;
  responseEnvelope?: string;
  outcomeRef?: string;
  requestID?: string;
  answerCode?: string;
}

/** A raw reply frame to a batch control request. */
export interface BatchCommandReply {
  type: string;
  payload?: unknown;
}
