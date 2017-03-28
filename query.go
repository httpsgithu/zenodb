package zenodb

import (
	"context"
	"github.com/getlantern/bytemap"
	"github.com/getlantern/zenodb/common"
	"github.com/getlantern/zenodb/core"
	"github.com/getlantern/zenodb/encoding"
	"github.com/getlantern/zenodb/planner"
	"time"
)

func (db *DB) Query(sqlString string, isSubQuery bool, subQueryResults [][]interface{}, includeMemStore bool) (core.FlatRowSource, error) {
	opts := &planner.Opts{
		GetTable: func(table string, includedFields func(tableFields core.Fields) core.Fields) planner.Table {
			return db.getQueryable(table, includedFields, includeMemStore)
		},
		Now:             db.now,
		FieldSource:     db.getFields,
		IsSubQuery:      isSubQuery,
		SubQueryResults: subQueryResults,
	}
	if db.opts.Passthrough {
		opts.QueryCluster = func(ctx context.Context, sqlString string, isSubQuery bool, subQueryResults [][]interface{}, unflat bool, onFields core.OnFields, onRow core.OnRow, onFlatRow core.OnFlatRow) error {
			return db.queryCluster(ctx, sqlString, isSubQuery, subQueryResults, includeMemStore, unflat, onFields, onRow, onFlatRow)
		}
	}
	plan, err := planner.Plan(sqlString, opts)
	if err != nil {
		return nil, err
	}
	log.Debugf("\n------------ Query Plan ------------\n\n%v\n\n%v\n----------- End Query Plan ----------", sqlString, core.FormatSource(plan))
	return plan, nil
}

func (db *DB) getQueryable(table string, includedFields func(tableFields core.Fields) core.Fields, includeMemStore bool) *queryable {
	t := db.getTable(table)
	if t == nil {
		return nil
	}
	until := encoding.RoundTimeUp(db.clock.Now(), t.Resolution)
	asOf := encoding.RoundTimeUp(until.Add(-1*t.RetentionPeriod), t.Resolution)
	return &queryable{t, includedFields(t.Fields), asOf, until, includeMemStore}
}

func MetaDataFor(source core.FlatRowSource, fields core.Fields) *common.QueryMetaData {
	return &common.QueryMetaData{
		FieldNames: fields.Names(),
		AsOf:       source.GetAsOf(),
		Until:      source.GetUntil(),
		Resolution: source.GetResolution(),
		Plan:       core.FormatSource(source),
	}
}

type queryable struct {
	t               *table
	fields          core.Fields
	asOf            time.Time
	until           time.Time
	includeMemStore bool
}

func (q *queryable) GetGroupBy() []core.GroupBy {
	return q.t.GroupBy
}

func (q *queryable) GetResolution() time.Duration {
	return q.t.Resolution
}

func (q *queryable) GetAsOf() time.Time {
	return q.asOf
}

func (q *queryable) GetUntil() time.Time {
	return q.until
}

func (q *queryable) GetPartitionBy() []string {
	return q.t.PartitionBy
}

func (q *queryable) String() string {
	return q.t.Name
}

func (q *queryable) Iterate(ctx context.Context, onFields core.OnFields, onRow core.OnRow) error {
	// We report all fields from the table
	onFields(q.t.Fields)

	// When iterating, as an optimization, we read only the needed fields (not
	// all table fields).
	return q.t.iterate(q.fields.Names(), q.includeMemStore, func(key bytemap.ByteMap, vals []encoding.Sequence) {
		onRow(key, vals)
	})
}
