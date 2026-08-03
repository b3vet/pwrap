// Package store wires OpenTelemetry into pgxpool via a minimal QueryTracer.
//
// Why custom and not e.g. otelpgx? Two reasons:
//   - We only need traces (no metrics yet) and the spans we want are dead simple
//     — query SQL + duration + error. Pulling otelpgx as a dep is overkill.
//   - pgx exposes a tiny Tracer interface; the implementation fits in 30 lines
//     and keeps SQL text trimming policy local (so we don't accidentally ship
//     pg_password into traces).
package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const tracerName = "pwrap/store"

// QueryTracer is the pgx hook. Implements pgx.QueryTracer.
type QueryTracer struct {
	tracer trace.Tracer
}

// NewQueryTracer returns a tracer ready for pgxpool.Config.ConnConfig.Tracer.
// The global trace provider is used; ensure telemetry.Init has run before any
// query if you want spans to actually export.
func NewQueryTracer() *QueryTracer {
	return &QueryTracer{tracer: otel.Tracer(tracerName)}
}

type queryCtxKey struct{}

// TraceQueryStart starts a child span around the query.
func (t *QueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	ctx, span := t.tracer.Start(ctx, "pgx.Query",
		trace.WithAttributes(
			attribute.String("db.system", "postgresql"),
			attribute.String("db.statement", truncateStatement(data.SQL)),
		),
	)
	return context.WithValue(ctx, queryCtxKey{}, span)
}

// TraceQueryEnd closes the span, recording the error if any.
func (t *QueryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	span, _ := ctx.Value(queryCtxKey{}).(trace.Span)
	if span == nil {
		return
	}
	defer span.End()
	if data.Err != nil {
		span.SetStatus(codes.Error, data.Err.Error())
		span.RecordError(data.Err)
	}
	span.SetAttributes(attribute.String("db.tag", data.CommandTag.String()))
}

// truncateStatement keeps trace payloads bounded and reduces the chance that a
// huge migration script blows up the exporter. 1KB is generous for telemetry.
func truncateStatement(sql string) string {
	const max = 1024
	if len(sql) <= max {
		return sql
	}
	return sql[:max] + "…"
}
