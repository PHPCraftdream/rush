import { Page } from '@playwright/test';
import { E2E_PREFIX } from "./e2e-prefix";

type ReceivedFrame = {
  type: string;
  payload: { sessions: Array<{ ID: string; Title?: string }> };
};

/** Browser-side: the frames the page has received from the mock WS. */
function readReceivedFrames() {
  return (window as unknown as Record<string, unknown>)["__wsReceived"] as Array<{
    type: string;
    payload: { sessions: Array<{ ID: string }> };
  }> | [];
}

/** Browser-side: asks the server to delete the session with the given ID. */
function sendDeleteSession(id: string) {
  const ws = ((window as unknown) as Record<string, unknown>)["__mockWS"] as WebSocket;
  if (ws && ws.readyState === 1) {
    ws.send(JSON.stringify({ type: "delete_session", payload: { sessionID: id } }));
  }
}

/** True for a sessions_list frame. */
function isSessionsList(frame: ReceivedFrame) {
  return frame.type === "sessions_list";
}

/** The sessions carried by a frame, or none. */
function sessionsOf(frame: ReceivedFrame) {
  return frame.payload?.sessions || [];
}

/** True for a session whose title carries the e2e prefix. */
function isE2ESession(session: { Title?: string }) {
  return session.Title?.startsWith(E2E_PREFIX);
}

/**
 * Clean up all e2e test sessions from the backend
 */
export async function cleanupE2ESessions(page: Page) {
  console.log("[E2E] Cleaning up test sessions...");

  // Get all sessions
  const sessions = await page.evaluate(readReceivedFrames) || [];

  const sessionsList = (sessions as ReceivedFrame[])
    .filter(isSessionsList)
    .flatMap(sessionsOf)
    .filter(isE2ESession);

  for (const session of sessionsList) {
    await page.evaluate(sendDeleteSession, session.ID);
  }

  // Wait a bit for deletion
  await page.waitForTimeout(500);
}
