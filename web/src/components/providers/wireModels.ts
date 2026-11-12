import type { ModelDraft } from "./ModelDraft";

/**
 * Maps one model draft to its wire shape, parsing the numeric fields.
 *
 * @param m - the draft
 */
const wireModel = (m: ModelDraft) => ({
  id: m.id,
  name: m.name,
  contextWindow: m.contextWindow ? parseInt(m.contextWindow, 10) : undefined,
  costPer1mIn: m.costPer1mIn ? parseFloat(m.costPer1mIn) : undefined,
  costPer1mOut: m.costPer1mOut ? parseFloat(m.costPer1mOut) : undefined,
});

/**
 * Maps model drafts to their wire shape.
 *
 * @param models - the drafts
 */
export function wireModels(models: readonly ModelDraft[]) {
  return models.map(wireModel);
}
