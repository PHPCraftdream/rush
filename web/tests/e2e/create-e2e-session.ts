import { Page } from '@playwright/test';
import { E2E_PREFIX } from "./e2e-prefix";

/** Browser-side: asks the server to create a session with the given title. */
function sendCreateSession(t: string) {
  const ws = ((window as unknown) as Record<string, unknown>)["__mockWS"] as WebSocket;
  if (!ws || ws.readyState !== 1) {
    throw new Error("WebSocket not connected");
  }
  ws.send(JSON.stringify({ type: "create_session", payload: { title: t } }));
  return null; // Session ID will come from session_created event
}

/** Browser-side: the frames the page has received from the mock WS. */
function readReceivedFrames() {
  return ((window as unknown) as Record<string, unknown>)["__wsReceived"] as Array<{
    type: string;
    payload: { sessions: Array<{ ID: string; Title: string }> };
  }>;
}

/** True for a frame that carries a sessions list or a created session. */
function carriesSessions(frame: { type: string }) {
  return frame.type === "sessions_list" || frame.type === "session_created";
}

/**
 * Create a test session with a unique name
 */
export async function createE2ESession(page: Page, title: string): Promise<string> {
  await page.evaluate(sendCreateSession, `${E2E_PREFIX}${title}`);

  // Wait for session_created event
  await page.waitForTimeout(1000);

  // Find the session ID from the sessions list
  const sessions = await page.evaluate(readReceivedFrames) || [];

  const session = sessions
    .find(carriesSessions)
    ?.payload?.sessions?.find((s: { Title: string }) => s.Title?.startsWith(E2E_PREFIX + title));

  if (!session) {
    throw new Error(`Failed to create session: ${E2E_PREFIX}${title}`);
  }

  console.log("[E2E] Created session:", session.ID, session.Title);
  return session.ID;
}
