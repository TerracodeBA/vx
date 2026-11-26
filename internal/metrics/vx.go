package metrics

// The metrics vx records. Label values are bounded: procedures, route
// patterns, status codes, results and limit classes; never repository,
// organization or person names.
var (
	RPCRequests = Default.NewCounter("vx_rpc_requests_total",
		"API calls by procedure and HTTP status (connect maps error codes to statuses: 429 is ResourceExhausted).", "procedure", "status")
	RPCDuration = Default.NewHistogram("vx_rpc_duration_seconds",
		"API call latency by procedure.", nil, "procedure")
	HTTPRequests = Default.NewCounter("vx_http_requests_total",
		"HTTP requests outside the API by route pattern, method and status.", "route", "method", "status")
	HTTPDuration = Default.NewHistogram("vx_http_request_duration_seconds",
		"HTTP request latency outside the API by route pattern.", nil, "route")

	Lands = Default.NewCounter("vx_lands_total",
		"Lands by result: landed, conflicts, review (waiting for a person), effort_moved, refused (scope, quota, rules) or error.", "result")
	LandConflictFiles = Default.NewCounter("vx_land_conflict_files_total",
		"Files that conflicted in lands.")
	Rebases = Default.NewCounter("vx_rebases_total",
		"Effort rebase events: started, done, conflicts (stopped on a conflict), aborted.", "event")
	Operations = Default.NewCounter("vx_operations_total",
		"Operation log entries written, by operation (effort.create, ship, org.member, ...).", "op")
	Forecasts = Default.NewCounter("vx_forecasts_total",
		"Background overlap forecast runs by result: ok or error.", "result")
	ForecastDuration = Default.NewHistogram("vx_forecast_duration_seconds",
		"Duration of one background overlap forecast run (one repository's queue).", nil)
	WebhookDeliveries = Default.NewCounter("vx_webhook_deliveries_total",
		"Webhook deliveries by host and HTTP status.", "host", "status")

	BlobOps = Default.NewCounter("vx_blob_ops_total",
		"Object store operations by operation (put, get, has) and result (ok, not_found, error).", "op", "result")
	BlobDuration = Default.NewHistogram("vx_blob_op_duration_seconds",
		"Object store operation latency.", []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 5}, "op")
	BlobBytes = Default.NewCounter("vx_blob_bytes_total",
		"Bytes read from and written to the object store (uncompressed object data).", "op")

	RateLimited = Default.NewCounter("vx_rate_limited_total",
		"Requests refused by a rate limit, by class: api_token, api_ip, signin, signup, cli_login, webhook.", "class")
	QuotaRefused = Default.NewCounter("vx_quota_refused_total",
		"Requests refused by an organization quota, by quota.", "quota")

	AuditPruned = Default.NewCounter("vx_audit_pruned_total",
		"Operation log entries deleted because they were older than the retention.")

	OrgsPurged = Default.NewCounter("vx_orgs_purged_total",
		"Deleted organizations whose data was purged.")
)

// RecordOp counts one operation log entry; store.LogOp calls it.
func RecordOp(op string) {
	Operations.Inc(op)
	switch op {
	case "effort.rebase":
		Rebases.Inc("started")
	case "effort.rebase.done":
		Rebases.Inc("done")
	case "effort.rebase.stop":
		Rebases.Inc("conflicts")
	case "effort.rebase.abort":
		Rebases.Inc("aborted")
	}
}
