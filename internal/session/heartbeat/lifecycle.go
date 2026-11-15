package heartbeat

// This file documents lifecycle ownership: workerOnce guarantees a single
// long-lived worker (workerStarts counts starts); hooks stay RWMutex-guarded.
