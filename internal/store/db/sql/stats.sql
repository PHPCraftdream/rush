-- name: GetUsageByModel :many
SELECT
    COALESCE(model, 'unknown') as model,
    COALESCE(provider, 'unknown') as provider,
    COUNT(*) as message_count
FROM messages
WHERE role = 'assistant'
GROUP BY model, provider
ORDER BY message_count DESC;

-- name: GetUsageByHour :many
SELECT
    CAST(strftime('%H', created_at, 'unixepoch') AS INTEGER) as hour,
    COUNT(*) as session_count
FROM sessions
WHERE parent_session_id IS NULL
GROUP BY hour
ORDER BY hour;

-- name: GetUsageByDayOfWeek :many
SELECT
    CAST(strftime('%w', created_at, 'unixepoch') AS INTEGER) as day_of_week,
    COUNT(*) as session_count,
    SUM(prompt_tokens) as prompt_tokens,
    SUM(completion_tokens) as completion_tokens
FROM sessions
WHERE parent_session_id IS NULL
GROUP BY day_of_week
ORDER BY day_of_week;

-- name: GetAverageResponseTime :one
SELECT
    CAST(COALESCE(AVG(finished_at - created_at), 0) AS INTEGER) as avg_response_seconds
FROM messages
WHERE role = 'assistant'
  AND finished_at IS NOT NULL
  AND finished_at > created_at;

-- name: GetToolUsage :many
SELECT
    json_extract(value, '$.data.name') as tool_name,
    COUNT(*) as call_count
FROM messages, json_each(parts)
WHERE json_extract(value, '$.type') = 'tool_call'
  AND json_extract(value, '$.data.name') IS NOT NULL
GROUP BY tool_name
ORDER BY call_count DESC;

-- name: GetHourDayHeatmap :many
SELECT
    CAST(strftime('%w', created_at, 'unixepoch') AS INTEGER) as day_of_week,
    CAST(strftime('%H', created_at, 'unixepoch') AS INTEGER) as hour,
    COUNT(*) as session_count
FROM sessions
WHERE parent_session_id IS NULL
GROUP BY day_of_week, hour
ORDER BY day_of_week, hour;
