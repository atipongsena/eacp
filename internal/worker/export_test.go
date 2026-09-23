package worker

import "context"

// Execute runs one leased action as the loop would (tests only).
func (w *Worker) Execute(ctx context.Context, l Lease) { w.execute(ctx, l) }
