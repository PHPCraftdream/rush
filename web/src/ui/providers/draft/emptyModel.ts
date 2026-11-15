import type { ModelDraft } from "../forms/ModelDraft";

/** A blank model draft. */
export function emptyModel(): ModelDraft {
  return { id: "", name: "", contextWindow: "", costPer1mIn: "", costPer1mOut: "" };
}
