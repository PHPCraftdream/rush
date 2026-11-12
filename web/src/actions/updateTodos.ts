import { ws } from "../net/client";
import { sessionsCarburetor } from "../state/sessions";
import { logClientEvent } from "../telemetry/logClientEvent";
import type { Todo } from "../types";

/**
 * The telemetry view of a todo: content and status only.
 *
 * @param t - the todo
 */
const todoSummary = (t: Todo): { content: string; status: string } => ({ content: t.content, status: t.status });

/**
 * Updates a session todos optimistically and sends them to the server.
 *
 * @param sessionID - session id
 * @param todos - the new todo list
 */
export function updateTodos(sessionID: string, todos: Todo[]): void {
  // Optimistic local update
  sessionsCarburetor.patchSession(sessionID, { Todos: todos });
  logClientEvent("update_todos", {
    sessionID,
    count: todos.length,
    todos: todos.map(todoSummary),
  });
  ws.send("update_todos", { sessionID, todos });
}
