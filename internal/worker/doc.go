// Package worker is the execution worker (MASTER_PLAN §79, ADR-004 T14,
// T16-T22a): it claims QUEUED actions from PostgreSQL, holds a fenced lease,
// records a dispatch intent before any external call, and commits the
// classified result fenced by its lease generation. Connector secrets live
// here and nowhere else (ADR-001 §3).
package worker
