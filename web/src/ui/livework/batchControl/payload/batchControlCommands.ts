/** WS command names of the batch control verbs. */
export const batchControlCommands = {
  status: "batch_status",
  output: "batch_output",
  stop: "batch_stop",
  add: "batch_add",
  drain: "batch_drain",
} as const;
