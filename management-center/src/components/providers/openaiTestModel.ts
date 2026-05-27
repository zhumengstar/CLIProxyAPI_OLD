import type { ModelEntry } from '@/components/ui/modelInputListUtils';

const FREE_MODEL_COST = -1;
const UNKNOWN_MODEL_COST = Number.POSITIVE_INFINITY;

const KNOWN_MODEL_COSTS: Array<[RegExp, number]> = [
  [/gpt-5\.4-mini/i, 0.75 + 4.5],
  [/gpt-5\.(2|3-codex)/i, 1.75 + 14],
  [/gpt-5\.4(?!-mini)/i, 2.5 + 15],
  [/gpt-5\.5/i, 5 + 30],
  [/gpt-4o-mini/i, 0.15 + 0.6],
  [/gpt-4\.1-mini/i, 0.4 + 1.6],
  [/gpt-4\.1-nano/i, 0.1 + 0.4],
  [/gpt-4\.1(?!-mini|-nano)/i, 2 + 8],
  [/gpt-4o(?!-mini)/i, 2.5 + 10],
  [/o4-mini/i, 1.1 + 4.4],
  [/o3-mini/i, 1.1 + 4.4],
  [/o3(?!-mini)/i, 2 + 8],
  [/deepseek[-/]?chat/i, 0.27 + 1.1],
  [/deepseek[-/]?reasoner/i, 0.55 + 2.19],
  [/kimi-k2/i, 0.6 + 2.5],
  [/qwen.*coder.*free/i, FREE_MODEL_COST],
  [/qwen.*free/i, FREE_MODEL_COST],
  [/llama.*free/i, FREE_MODEL_COST],
];

export const estimateOpenAIModelTestCost = (modelName: string): number => {
  const normalized = modelName.trim().toLowerCase();
  if (!normalized) return UNKNOWN_MODEL_COST;
  if (normalized.includes(':free') || normalized.endsWith('/free') || normalized.includes('-free')) {
    return FREE_MODEL_COST;
  }
  const match = KNOWN_MODEL_COSTS.find(([pattern]) => pattern.test(normalized));
  return match ? match[1] : UNKNOWN_MODEL_COST;
};

export const selectCheapestOpenAIModelName = (
  entries: ModelEntry[],
  fallbackModels: string[] = []
): string => {
  const seen = new Set<string>();
  const candidates: string[] = [];

  entries.forEach((entry) => {
    const name = entry.name.trim();
    if (!name || seen.has(name)) return;
    seen.add(name);
    candidates.push(name);
  });

  if (candidates.length === 0) {
    fallbackModels.forEach((model) => {
      const name = model.trim();
      if (!name || seen.has(name)) return;
      seen.add(name);
      candidates.push(name);
    });
  }

  if (candidates.length === 0) return '';

  return candidates.reduce((cheapest, candidate) => {
    const candidateCost = estimateOpenAIModelTestCost(candidate);
    const cheapestCost = estimateOpenAIModelTestCost(cheapest);
    if (candidateCost < cheapestCost) return candidate;
    if (candidateCost === cheapestCost && candidate.localeCompare(cheapest) < 0) return candidate;
    return cheapest;
  }, candidates[0]);
};
