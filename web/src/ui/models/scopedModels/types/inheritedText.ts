import type { ModelOverrideWire } from "./scopedModelsTypes";

/**
 * The hint shown under an unset slot: what it inherits and from where.
 *
 * @param effective - the effective override, if any
 * @param scope - the scope it comes from
 */
export function inheritedText(effective: ModelOverrideWire | null | undefined, scope: string | undefined): string {
  if (!effective) return "not set in any scope";
  return `inherited: ${effective.provider}/${effective.model}${scope ? ` (from ${scope})` : ""}`;
}
