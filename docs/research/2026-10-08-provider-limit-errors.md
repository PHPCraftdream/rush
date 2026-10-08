# Provider limit / billing errors seen in the wild

Purpose: input for the shared "hard limit" classifier (`agent.IsHardQuotaLimit`,
`isQuotaLimit`, `QuotaLimitResetTime`) and the `provider_limit` exit of `rush run`.

**Evidence level.** Only the Anthropic row 1 was captured on a Rush machine
(2026-10-08). Everything else comes from the vendors' own docs or from public
bug reports and was NOT reproduced here; fixtures built from it must say so and
cite the source. Formats can change without notice, so detection works on
keywords with provider-aware exceptions, never on one exact string.

Terms. **Hard** = retrying cannot help until money/plan/window changes (stop the
run's own turns, drain live work, exit `provider_limit`). **Transient** = rate
limit or overload, retry/wait on the existing budget. **Ambiguous** = the same
words mean different things per provider (see the notes).

## Rule of thumb for the classifier

1. A *hard* keyword/code on a failure channel (HTTP error message/body, CLI
   stderr, CLI failure events) => hard.
2. A *transient* marker next to it ("per minute", "TPM", "RPM", "requests",
   "overloaded", "try again later", 5xx) => transient, unless rule 3.
3. A reset / `Retry-After` of 30 minutes or more => hard: the rate-limit wait
   budget is 5 min per wait / 30 min total, so such a wait can never succeed.
   (Azure OpenAI is reported to send 86400 s.)
4. Provider-specific overrides win over the generic words (DashScope below).
5. Never scan successful turns or assistant/tool text.

## Per provider

| Provider | HTTP / shape | Hard | Transient | Reset info |
|---|---|---|---|---|
| Anthropic API | **captured**: 400 `invalid_request_error`, "Your credit balance is too low to access the Anthropic API. Please go to Plans & Billing to upgrade or purchase credits." (docs also list 402 `billing_error`) | credit balance too low | 429 `rate_limit_error`, 529 overloaded | `anthropic-ratelimit-*-reset` headers |
| Claude CLI | text variants: "Claude AI usage limit reached\|<epoch>", "Claude usage limit reached. Your limit will reset at 3pm (TZ)", "You've hit your limit · resets 4pm (Asia/Kuala_Lumpur)"; headless failures arrive as a final stream-json `{"type":"result","is_error":true,...}` line | usage limit reached / hit your limit | one report shows the same wording over an API 429 `rate_limit_error` "This request would exceed your account's rate limit" at 84% usage | epoch after `\|`, "resets 4pm (IANA tz)" |
| Codex CLI (`exec --json`) | stdout JSONL `{"type":"error","message":"You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at Oct 9th, 2026 11:19 PM."}` then `{"type":"turn.failed","error":{"message":"You've hit your usage limit. ..."}}`; exit 1; stderr only "Reading additional input from stdin..." (codex-cli 0.135, 0.142) | hit your usage limit | – | "try again at Oct 9th, 2026 11:19 PM" (no tz, local) |
| OpenAI Codex / ChatGPT backend (rush `openai-codex`) | HTTP 429, `error.type` `usage_limit_reached`, "The usage limit has been reached", `plan_type`, `resets_at` (epoch), `resets_in_seconds`, `limit_window_minutes` (300, null for separate caps) | usage_limit_reached | `rate_limit_exceeded` | `resets_in_seconds`, `resets_at` |
| OpenAI API | 429 `insufficient_quota` "You exceeded your current quota, please check your plan and billing details" | insufficient_quota | `rate_limit_exceeded` | – |
| Azure OpenAI | 429 "…have exceeded token rate limit of your current OpenAI S0 pricing tier…"; message says "quota" but is usually a TPM limit; `Retry-After` up to 86400 reported | only by long Retry-After (rule 3) | token rate limit | `Retry-After` |
| Z.ai / GLM | 429 + business `code` in body: **1113** insufficient balance / no resource package; **1308** "Usage limit reached for {n} {unit}. Your limit will reset at {time}."; **1309** coding plan expired; **1310** weekly/monthly limit exhausted (reset time given); **1316/1317** 5h/7d limit and balance insufficient; **1318–1321** limit reached and extra usage blocked by monthly spend limit. Coding-plan keys on the general `/api/paas/v4` endpoint get 1113 | 1113, 1308, 1309, 1310, 1316–1321 | **1302** rate limit for requests; **1305** overloaded | "Your limit will reset at …" in body (existing parser: `zaiResetRe`) |
| Moonshot / Kimi | 429, `error.type`: `exceeded_current_quota_error` ("…suspended due to insufficient balance, please recharge your account or check your plan and billing details"), `rate_limit_reached_error`, `engine_overloaded_error` | exceeded_current_quota_error | rate_limit_reached_error, engine_overloaded_error | `Retry-After` |
| MiniMax | business codes: **1008** insufficient balance; **2056** usage limit exceeded (wait for next 5-hour window); **1002** rate limit. Reports of false 1008 on coding plans (500 "insufficient balance (1008)") | 1008, 2056 | 1002 | message |
| DeepSeek | official: **402** Insufficient Balance; **429** Rate Limit Reached; **503** Server Overloaded | 402 | 429, 503 | – |
| Qwen / Alibaba DashScope (Model Studio) | 429 codes: `Throttling.RateQuota`/`LimitRequests` (rate), **`Throttling.AllocationQuota` / `insufficient_quota` = temporary TPS/TPM throttle despite the word "quota"**; coding plan "hour/week/month allocated quota exceeded" = plan window used up; billing: `PrepaidBillOverdue`, `PostpaidBillOverdue`, "Free allocated quota exceeded", Arrearage (overdue) | plan-window and billing codes above | `Throttling.*`, `insufficient_quota` | – |
| StepFun | official: **402** insufficient balance; **429** rate/resource limit (also Step Plan periodic/concurrency limits) | 402 | 429 | – |
| OpenRouter | **402** "Insufficient credits. This account never purchased credits…"; 429 free-model limits (20 rpm, 50/day or 1000/day after $10 lifetime purchase; failed requests count) | 402 | 429 per-minute / upstream | – |
| xAI | 429 for two reasons: "used all available credits or reached monthly spending limit" (hard) vs per-model RPS/TPM (transient) | credits / spending limit | rate | – |
| Groq | 429 `rate_limit_exceeded` naming TPD (daily) or TPM; text "Please try again in 9m38s" | tokens per day (TPD) | TPM / RPM | "try again in …" |
| Gemini API | 429 `RESOURCE_EXHAUSTED`, `QuotaFailure.violations[].quotaId` (`…PerDay…` = daily, `…PerMinute…` = minute), `RetryInfo.retryDelay` | `…PerDay…` quotaId | `…PerMinute…`, token-per-minute | `retryDelay` (only meaningful for per-minute) |
| Mistral | 429 `{"message":"Rate limit exceeded","type":"rate_limited","code":"1300"}` / "Requests rate limit exceeded"; monthly token limits exist | none evidenced | generic 429 | – |
| AWS Bedrock | 429 `ThrottlingException` "Too many requests"; one thread: "Too many tokens per day" | tokens per day | other throttling | `Retry-After` sometimes |

## Sources

- Anthropic row 1: captured locally on 2026-10-08 (HTTP 400 body).
- Claude CLI: anthropics/claude-code issues #2087, #9236, #19673; stream-json `is_error`: codingworkflow/claude-code-api PR #48.
- Codex CLI events: agent-grounds/rhei #471, zhupanov/larch #3380, openai/codex #29948, #16909.
- ChatGPT backend `usage_limit_reached` fields: acmiyaguchi/fen #583, NousResearch/hermes-agent #26889 and #95066, openai/codex #49214.
- Z.ai: https://docs.z.ai/api-reference/api-code and https://docs.z.ai/devpack/faq (official); zai-org/feedback #789.
- Kimi: https://platform.kimi.ai/docs/guide/troubleshooting (official); meridianlabs-ai/inspect_ai #454; anomalyco/opencode #24462.
- MiniMax: https://platform.minimax.io/docs/api-reference/errorcode (official).
- DeepSeek: https://api-docs.deepseek.com/quick_start/error_codes/ (official).
- DashScope: https://www.alibabacloud.com/help/en/model-studio/error-code (official); earendil-works/pi #10656; openclaw/openclaw #148236.
- StepFun: https://platform.stepfun.ai/docs/en/api-reference/error-codes (official).
- OpenAI `insufficient_quota`: OpenAI developer community threads 649547, 492350.
- Azure: Microsoft Q&A threads on "exceeded token rate limit".
- Gemini: Google AI developer forum and google-gemini/gemini-cli #8883.
- OpenRouter, xAI, Groq, Mistral, Bedrock: public issue trackers and vendor docs; see the search queries in the task #1282/#1283 notes.
