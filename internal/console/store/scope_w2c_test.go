package store

// addW2CMatrix adds the M2-W2C methods to the §3.5 scope matrix (matrix() in
// scope_test.go calls it): the audited command queue, command cancel, the
// command history and the identity-conflict resolution run as an all-customer
// admin, an operator scoped to A, a read-only user scoped to B and the zero
// Scope. A denied call is ErrNotFound and leaves the fingerprint (which holds
// the device row and its commands) unchanged.
func addW2CMatrix(m map[string]matrixCase) {
	actor := AuditActor{AdminID: "01J9Y4K3A0B1C2D3E4F5G6H7AA", Username: "matrix", Method: "api"}
	m["QueueCommandAudited"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
		_, err := w.s.QueueCommandAudited(w.ctx, sc, t.dev, CommandInput{Type: "check_in", IdempotencyKey: uniq("w2c")}, actor)
		return nil, err
	}}
	m["CancelCommandAudited"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
		_, _, err := w.s.CancelCommandAudited(w.ctx, sc, t.dev, t.command, actor)
		return nil, err
	}, prep: func(w *world, t *target) {
		// A queued command to cancel, fresh for every principal (the seeded one is cancelled by the first allowed call).
		cmd, _, err := w.s.EnqueueCommand(w.ctx, ScopeAll(), t.dev, CommandInput{Type: "check_in", IdempotencyKey: uniq("w2c-prep")})
		ok(w.t, err)
		t.command = cmd.ID
	}}
	m["ListDeviceCommands"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
		p, err := w.s.ListDeviceCommands(w.ctx, sc, t.dev, CommandFilter{}, w.clk.Now(), 1, 25, 0, 0)
		if err == nil && len(p.Items) == 0 {
			err = ErrNotFound
		}
		return nil, err
	}}
	m["ResolveIdentityConflict"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
		_, err := w.s.ResolveIdentityConflict(w.ctx, sc, t.dev, IdentityResolution{Action: "keep", Reason: "matrix"}, actor)
		return nil, err
	}, prep: func(w *world, t *target) { w.flagConflict(t.dev) }}
}
