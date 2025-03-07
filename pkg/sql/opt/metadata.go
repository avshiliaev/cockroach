// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package opt

import (
	"context"
	"fmt"
	"math/bits"
	"strings"

	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/multiregion"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/cat"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/privilege"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/catid"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/eval"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/intsets"
	"github.com/cockroachdb/errors"
	"github.com/lib/pq/oid"
)

// SchemaID uniquely identifies the usage of a schema within the scope of a
// query. SchemaID 0 is reserved to mean "unknown schema". Internally, the
// SchemaID consists of an index into the Metadata.schemas slice.
//
// See the comment for Metadata for more details on identifiers.
type SchemaID int32

// privilegeBitmap stores a union of zero or more privileges. Each privilege
// that is present in the bitmap is represented by a bit that is shifted by
// 1 << privilege.Kind, so that multiple privileges can be stored.
type privilegeBitmap uint64

// Metadata assigns unique ids to the columns, tables, and other metadata used
// for global identification within the scope of a particular query. These ids
// tend to be small integers that can be efficiently stored and manipulated.
//
// Within a query, every unique column and every projection should be assigned a
// unique column id. Additionally, every separate reference to a table in the
// query should get a new set of output column ids.
//
// For example, consider the query:
//
//	SELECT x FROM a WHERE y > 0
//
// There are 2 columns in the above query: x and y. During name resolution, the
// above query becomes:
//
//	SELECT [0] FROM a WHERE [1] > 0
//	-- [0] -> x
//	-- [1] -> y
//
// An operator is allowed to reuse some or all of the column ids of an input if:
//
//  1. For every output row, there exists at least one input row having identical
//     values for those columns.
//  2. OR if no such input row exists, there is at least one output row having
//     NULL values for all those columns (e.g. when outer join NULL-extends).
//
// For example, is it safe for a Select to use its input's column ids because it
// only filters rows. Likewise, pass-through column ids of a Project can be
// reused.
//
// For an example where columns cannot be reused, consider the query:
//
//	SELECT * FROM a AS l JOIN a AS r ON (l.x = r.y)
//
// In this query, `l.x` is not equivalent to `r.x` and `l.y` is not equivalent
// to `r.y`. Therefore, we need to give these columns different ids.
type Metadata struct {
	// schemas stores each schema used by the query if it is a CREATE statement,
	// indexed by SchemaID.
	schemas []cat.Schema

	// cols stores information about each metadata column, indexed by
	// ColumnID.index().
	cols []ColumnMeta

	// tables stores information about each metadata table, indexed by
	// TableID.index().
	tables []TableMeta

	// sequences stores information about each metadata sequence, indexed by SequenceID.
	sequences []cat.Sequence

	// userDefinedTypes contains all user defined types present in expressions
	// in this query.
	// TODO (rohany): This only contains user defined types present in the query
	//  because the installation of type metadata in tables doesn't go through
	//  the type resolver that the optimizer hijacks. However, we could update
	//  this map when adding a table via metadata.AddTable.
	userDefinedTypes      map[oid.Oid]struct{}
	userDefinedTypesSlice []*types.T

	// views stores the list of referenced views. This information is only
	// needed for EXPLAIN (opt, env).
	views []cat.View

	// currUniqueID is the highest UniqueID that has been assigned.
	currUniqueID UniqueID

	// withBindings store bindings for relational expressions inside With or
	// mutation operators, used to determine the logical properties of WithScan.
	withBindings map[WithID]Expr

	// dataSourceDeps stores each data source object that the query depends on.
	dataSourceDeps map[cat.StableID]cat.DataSource

	// udfDeps stores each user-defined function overload that the query depends
	// on.
	udfDeps map[cat.StableID]*tree.Overload

	// objectRefsByName stores each unique name that the query uses to reference
	// each object. It is needed because changes to the search path may change
	// which object a given name refers to; for example, switching the database.
	objectRefsByName map[cat.StableID][]*tree.UnresolvedObjectName

	// privileges stores the privileges needed to access each object that the
	// query depends on.
	privileges map[cat.StableID]privilegeBitmap

	// builtinRefsByName stores the names used to reference builtin functions in
	// the query. This is necessary to handle the case where changes to the search
	// path cause a function call to be resolved to a UDF with the same signature
	// as a builtin function.
	builtinRefsByName map[tree.UnresolvedName]struct{}

	// NOTE! When adding fields here, update Init (if reusing allocated
	// data structures is desired), CopyFrom and TestMetadata.
}

// Init prepares the metadata for use (or reuse).
func (md *Metadata) Init() {
	// Clear the metadata objects to release memory (this clearing pattern is
	// optimized by Go).
	schemas := md.schemas
	for i := range schemas {
		schemas[i] = nil
	}

	cols := md.cols
	for i := range cols {
		cols[i] = ColumnMeta{}
	}

	tables := md.tables
	for i := range tables {
		tables[i] = TableMeta{}
	}

	sequences := md.sequences
	for i := range sequences {
		sequences[i] = nil
	}

	views := md.views
	for i := range views {
		views[i] = nil
	}

	dataSourceDeps := md.dataSourceDeps
	if dataSourceDeps == nil {
		dataSourceDeps = make(map[cat.StableID]cat.DataSource)
	}
	for id := range md.dataSourceDeps {
		delete(md.dataSourceDeps, id)
	}

	udfDeps := md.udfDeps
	if udfDeps == nil {
		udfDeps = make(map[cat.StableID]*tree.Overload)
	}
	for id := range md.udfDeps {
		delete(md.udfDeps, id)
	}

	objectRefsByName := md.objectRefsByName
	if objectRefsByName == nil {
		objectRefsByName = make(map[cat.StableID][]*tree.UnresolvedObjectName)
	}
	for id := range md.objectRefsByName {
		delete(md.objectRefsByName, id)
	}

	privileges := md.privileges
	if privileges == nil {
		privileges = make(map[cat.StableID]privilegeBitmap)
	}
	for id := range md.privileges {
		delete(md.privileges, id)
	}

	builtinRefsByName := md.builtinRefsByName
	if builtinRefsByName == nil {
		builtinRefsByName = make(map[tree.UnresolvedName]struct{})
	}
	for name := range md.builtinRefsByName {
		delete(md.builtinRefsByName, name)
	}

	// This initialization pattern ensures that fields are not unwittingly
	// reused. Field reuse must be explicit.
	*md = Metadata{}
	md.schemas = schemas[:0]
	md.cols = cols[:0]
	md.tables = tables[:0]
	md.sequences = sequences[:0]
	md.views = views[:0]
	md.dataSourceDeps = dataSourceDeps
	md.udfDeps = udfDeps
	md.objectRefsByName = objectRefsByName
	md.privileges = privileges
	md.builtinRefsByName = builtinRefsByName
}

// CopyFrom initializes the metadata with a copy of the provided metadata.
// This metadata can then be modified independent of the copied metadata.
//
// Table annotations are not transferred over; all annotations are unset on
// the copy, except for regionConfig, which is read-only, and can be shared.
//
// copyScalarFn must be a function that returns a copy of the given scalar
// expression.
func (md *Metadata) CopyFrom(from *Metadata, copyScalarFn func(Expr) Expr) {
	if len(md.schemas) != 0 || len(md.cols) != 0 || len(md.tables) != 0 ||
		len(md.sequences) != 0 || len(md.views) != 0 || len(md.userDefinedTypes) != 0 ||
		len(md.userDefinedTypesSlice) != 0 || len(md.dataSourceDeps) != 0 ||
		len(md.udfDeps) != 0 || len(md.objectRefsByName) != 0 || len(md.privileges) != 0 ||
		len(md.builtinRefsByName) != 0 {
		panic(errors.AssertionFailedf("CopyFrom requires empty destination"))
	}
	md.schemas = append(md.schemas, from.schemas...)
	md.cols = append(md.cols, from.cols...)

	if len(from.userDefinedTypesSlice) > 0 {
		if md.userDefinedTypes == nil {
			md.userDefinedTypes = make(map[oid.Oid]struct{}, len(from.userDefinedTypesSlice))
		}
		for i := range from.userDefinedTypesSlice {
			typ := from.userDefinedTypesSlice[i]
			md.userDefinedTypes[typ.Oid()] = struct{}{}
			md.userDefinedTypesSlice = append(md.userDefinedTypesSlice, typ)
		}
	}

	if cap(md.tables) >= len(from.tables) {
		md.tables = md.tables[:len(from.tables)]
	} else {
		md.tables = make([]TableMeta, len(from.tables))
	}
	for i := range from.tables {
		// Note: annotations inside TableMeta are not retained...
		md.tables[i].copyFrom(&from.tables[i], copyScalarFn)

		// ...except for the regionConfig annotation.
		tabID := from.tables[i].MetaID
		regionConfig, ok := md.TableAnnotation(tabID, regionConfigAnnID).(*multiregion.RegionConfig)
		if ok {
			// Don't waste time looking up a database descriptor and constructing a
			// RegionConfig more than once for a given table.
			md.SetTableAnnotation(tabID, regionConfigAnnID, regionConfig)
		}
	}

	for id, dataSource := range from.dataSourceDeps {
		if md.dataSourceDeps == nil {
			md.dataSourceDeps = make(map[cat.StableID]cat.DataSource)
		}
		md.dataSourceDeps[id] = dataSource
	}

	for id, overload := range from.udfDeps {
		if md.udfDeps == nil {
			md.udfDeps = make(map[cat.StableID]*tree.Overload)
		}
		md.udfDeps[id] = overload
	}

	for id, names := range from.objectRefsByName {
		if md.objectRefsByName == nil {
			md.objectRefsByName = make(map[cat.StableID][]*tree.UnresolvedObjectName)
		}
		newNames := make([]*tree.UnresolvedObjectName, len(names))
		copy(newNames, names)
		md.objectRefsByName[id] = newNames
	}

	for id, privilegeSet := range from.privileges {
		if md.privileges == nil {
			md.privileges = make(map[cat.StableID]privilegeBitmap)
		}
		md.privileges[id] = privilegeSet
	}

	for name := range from.builtinRefsByName {
		if md.builtinRefsByName == nil {
			md.builtinRefsByName = make(map[tree.UnresolvedName]struct{})
		}
		md.builtinRefsByName[name] = struct{}{}
	}

	md.sequences = append(md.sequences, from.sequences...)
	md.views = append(md.views, from.views...)
	md.currUniqueID = from.currUniqueID

	// We cannot copy the bound expressions; they must be rebuilt in the new memo.
	md.withBindings = nil
}

// MDDepName stores either the unresolved DataSourceName or the StableID from
// the query that was used to resolve a data source.
type MDDepName struct {
	// byID is non-zero if and only if the data source was looked up using the
	// StableID.
	byID cat.StableID

	// byName is non-zero if and only if the data source was looked up using a
	// name.
	byName cat.DataSourceName
}

// DepByName is used with AddDependency when the data source was looked up using a
// data source name.
func DepByName(name *cat.DataSourceName) MDDepName {
	return MDDepName{byName: *name}
}

// DepByID is used with AddDependency when the data source was looked up by ID.
func DepByID(id cat.StableID) MDDepName {
	return MDDepName{byID: id}
}

// AddDependency tracks one of the catalog data sources on which the query
// depends, as well as the privilege required to access that data source. If
// the Memo using this metadata is cached, then a call to CheckDependencies can
// detect if the name resolves to a different data source now, or if changes to
// schema or permissions on the data source has invalidated the cached metadata.
func (md *Metadata) AddDependency(name MDDepName, ds cat.DataSource, priv privilege.Kind) {
	id := ds.ID()
	md.dataSourceDeps[id] = ds
	md.privileges[id] = md.privileges[id] | (1 << priv)
	if name.byID == 0 {
		// This data source was referenced by name.
		md.objectRefsByName[id] = append(md.objectRefsByName[id], name.byName.ToUnresolvedObjectName())
	}
}

// CheckDependencies resolves (again) each database object on which this
// metadata depends, in order to check the following conditions:
//  1. The object has not been modified.
//  2. If referenced by name, the name does not resolve to a different object.
//  3. The user still has sufficient privileges to access the object. Note that
//     this point currently only applies to data sources.
//
// If the dependencies are no longer up-to-date, then CheckDependencies returns
// false.
//
// This function can only swallow "undefined" or "dropped" errors, since these
// are expected. Other error types must be propagated, since CheckDependencies
// may perform KV operations on behalf of the transaction associated with the
// provided catalog.
func (md *Metadata) CheckDependencies(
	ctx context.Context, evalCtx *eval.Context, optCatalog cat.Catalog,
) (upToDate bool, err error) {
	// Check that no referenced data sources have changed.
	for id, dataSource := range md.dataSourceDeps {
		var toCheck cat.DataSource
		if names, ok := md.objectRefsByName[id]; ok {
			// The data source was referenced by name at least once.
			for _, name := range names {
				tableName := name.ToTableName()
				toCheck, _, err = optCatalog.ResolveDataSource(ctx, cat.Flags{}, &tableName)
				if err != nil || !dataSource.Equals(toCheck) {
					return false, maybeSwallowMetadataResolveErr(err)
				}
			}
		} else {
			// The data source was only referenced by ID.
			toCheck, _, err = optCatalog.ResolveDataSourceByID(ctx, cat.Flags{}, dataSource.ID())
			if err != nil || !dataSource.Equals(toCheck) {
				return false, maybeSwallowMetadataResolveErr(err)
			}
		}
	}

	// Ensure that all required privileges for the data sources are still valid.
	if err := md.checkDataSourcePrivileges(ctx, optCatalog); err != nil {
		return false, err
	}

	// Check that no referenced user defined types have changed.
	for _, typ := range md.AllUserDefinedTypes() {
		id := cat.StableID(catid.UserDefinedOIDToID(typ.Oid()))
		if names, ok := md.objectRefsByName[id]; ok {
			for _, name := range names {
				toCheck, err := optCatalog.ResolveType(ctx, name)
				if err != nil || typ.Oid() != toCheck.Oid() ||
					typ.TypeMeta.Version != toCheck.TypeMeta.Version {
					return false, maybeSwallowMetadataResolveErr(err)
				}
			}
		} else {
			toCheck, err := optCatalog.ResolveTypeByOID(ctx, typ.Oid())
			if err != nil || typ.TypeMeta.Version != toCheck.TypeMeta.Version {
				return false, maybeSwallowMetadataResolveErr(err)
			}
		}
	}

	// Check that no referenced user defined functions have changed.
	for id, overload := range md.udfDeps {
		if names, ok := md.objectRefsByName[id]; ok {
			for _, name := range names {
				definition, err := optCatalog.ResolveFunction(
					ctx, name.ToUnresolvedName(), &evalCtx.SessionData().SearchPath,
				)
				if err != nil {
					return false, maybeSwallowMetadataResolveErr(err)
				}
				toCheck, err := definition.MatchOverload(overload.Types.Types(), name.Schema(), &evalCtx.SessionData().SearchPath)
				if err != nil || toCheck.Oid != overload.Oid || toCheck.Version != overload.Version {
					return false, err
				}
			}
		} else {
			_, toCheck, err := optCatalog.ResolveFunctionByOID(ctx, overload.Oid)
			if err != nil || overload.Version != toCheck.Version {
				return false, maybeSwallowMetadataResolveErr(err)
			}
		}
	}

	// Check that the role still has execution privilege on the user defined
	// functions.
	for _, overload := range md.udfDeps {
		if err := optCatalog.CheckExecutionPrivilege(ctx, overload.Oid); err != nil {
			return false, err
		}
	}

	// Check that any references to builtin functions do not now resolve to a UDF
	// with the same signature (e.g. after changes to the search path).
	for name := range md.builtinRefsByName {
		definition, err := optCatalog.ResolveFunction(
			ctx, &name, &evalCtx.SessionData().SearchPath,
		)
		if err != nil {
			return false, maybeSwallowMetadataResolveErr(err)
		}
		for i := range definition.Overloads {
			if definition.Overloads[i].Type == tree.UDFRoutine {
				return false, nil
			}
		}
	}

	return true, nil
}

// handleMetadataResolveErr swallows errors that are thrown when a database
// object is dropped, since such an error potentially only means that the
// metadata is stale and should be re-resolved.
func maybeSwallowMetadataResolveErr(err error) error {
	if err == nil {
		return nil
	}
	// Handle when the object no longer exists.
	switch pgerror.GetPGCode(err) {
	case pgcode.UndefinedObject, pgcode.UndefinedTable, pgcode.UndefinedDatabase,
		pgcode.UndefinedSchema, pgcode.UndefinedFunction, pgcode.InvalidName,
		pgcode.InvalidSchemaName, pgcode.InvalidCatalogName:
		return nil
	}
	if errors.Is(err, catalog.ErrDescriptorDropped) {
		return nil
	}
	return err
}

// checkDataSourcePrivileges checks that none of the privileges required by the
// query for the referenced data sources have been revoked.
func (md *Metadata) checkDataSourcePrivileges(ctx context.Context, optCatalog cat.Catalog) error {
	for _, dataSource := range md.dataSourceDeps {
		privileges := md.privileges[dataSource.ID()]
		for privs := privileges; privs != 0; {
			// Strip off each privilege bit and make call to CheckPrivilege for it.
			// Note that priv == 0 can occur when a dependency was added with
			// privilege.Kind = 0 (e.g. for a table within a view, where the table
			// privileges do not need to be checked). Ignore the "zero privilege".
			priv := privilege.Kind(bits.TrailingZeros32(uint32(privs)))
			if priv != 0 {
				if err := optCatalog.CheckPrivilege(ctx, dataSource, priv); err != nil {
					return err
				}
			}
			// Set the just-handled privilege bit to zero and look for next.
			privs &= ^(1 << priv)
		}
	}
	return nil
}

// AddSchema indexes a new reference to a schema used by the query.
func (md *Metadata) AddSchema(sch cat.Schema) SchemaID {
	md.schemas = append(md.schemas, sch)
	return SchemaID(len(md.schemas))
}

// Schema looks up the metadata for the schema associated with the given schema
// id.
func (md *Metadata) Schema(schID SchemaID) cat.Schema {
	return md.schemas[schID-1]
}

// AddUserDefinedType adds a user defined type to the metadata for this query.
// If the type was resolved by name, the name will be tracked as well.
func (md *Metadata) AddUserDefinedType(typ *types.T, name *tree.UnresolvedObjectName) {
	if !typ.UserDefined() {
		return
	}
	if md.userDefinedTypes == nil {
		md.userDefinedTypes = make(map[oid.Oid]struct{})
	}
	if _, ok := md.userDefinedTypes[typ.Oid()]; !ok {
		md.userDefinedTypes[typ.Oid()] = struct{}{}
		md.userDefinedTypesSlice = append(md.userDefinedTypesSlice, typ)
	}
	if name != nil {
		id := cat.StableID(catid.UserDefinedOIDToID(typ.Oid()))
		md.objectRefsByName[id] = append(md.objectRefsByName[id], name)
	}
}

// AllUserDefinedTypes returns all user defined types contained in this query.
func (md *Metadata) AllUserDefinedTypes() []*types.T {
	return md.userDefinedTypesSlice
}

// HasUserDefinedFunctions returns true if the query references a UDF.
func (md *Metadata) HasUserDefinedFunctions() bool {
	return len(md.udfDeps) > 0
}

// AddUserDefinedFunction adds a user-defined function to the metadata for this
// query. If the function was resolved by name, the name will also be tracked.
func (md *Metadata) AddUserDefinedFunction(
	overload *tree.Overload, name *tree.UnresolvedObjectName,
) {
	if overload.Type != tree.UDFRoutine {
		return
	}
	id := cat.StableID(catid.UserDefinedOIDToID(overload.Oid))
	md.udfDeps[id] = overload
	if name != nil {
		md.objectRefsByName[id] = append(md.objectRefsByName[id], name)
	}
}

// AddBuiltin adds a name used to resolve a builtin function to the metadata for
// this query. This is necessary to handle the case when changes to the search
// path cause a function call to resolve as a UDF instead of a builtin function.
func (md *Metadata) AddBuiltin(name *tree.UnresolvedObjectName) {
	if name == nil {
		return
	}
	if md.builtinRefsByName == nil {
		md.builtinRefsByName = make(map[tree.UnresolvedName]struct{})
	}
	md.builtinRefsByName[*name.ToUnresolvedName()] = struct{}{}
}

// AddTable indexes a new reference to a table within the query. Separate
// references to the same table are assigned different table ids (e.g.  in a
// self-join query). All columns are added to the metadata. If mutation columns
// are present, they are added after active columns.
//
// The ExplicitCatalog/ExplicitSchema fields of the table's alias are honored so
// that its original formatting is preserved for error messages,
// pretty-printing, etc.
func (md *Metadata) AddTable(tab cat.Table, alias *tree.TableName) TableID {
	tabID := makeTableID(len(md.tables), ColumnID(len(md.cols)+1))
	if md.tables == nil {
		md.tables = make([]TableMeta, 0, 4)
	}
	md.tables = append(md.tables, TableMeta{MetaID: tabID, Table: tab, Alias: *alias})

	colCount := tab.ColumnCount()
	if md.cols == nil {
		md.cols = make([]ColumnMeta, 0, colCount)
	}

	for i := 0; i < colCount; i++ {
		col := tab.Column(i)
		colID := md.AddColumn(string(col.ColName()), col.DatumType())
		md.ColumnMeta(colID).Table = tabID
	}

	return tabID
}

// DuplicateTable creates a new reference to the table with the given ID. All
// columns are added to the metadata with new column IDs. If mutation columns
// are present, they are added after active columns. The ID of the new table
// reference is returned. This function panics if a table with the given ID does
// not exists in the metadata.
//
// remapColumnIDs must be a function that remaps the column IDs within a
// ScalarExpr to new column IDs. It takes as arguments a ScalarExpr and a
// mapping of old column IDs to new column IDs, and returns a new ScalarExpr.
// This function is used when duplicating Constraints, ComputedCols, and
// partialIndexPredicates. DuplicateTable requires this callback function,
// rather than performing the remapping itself, because remapping column IDs
// requires constructing new expressions with norm.Factory. The norm package
// depends on opt, and cannot be imported here.
//
// The ExplicitCatalog/ExplicitSchema fields of the table's alias are honored so
// that its original formatting is preserved for error messages,
// pretty-printing, etc.
func (md *Metadata) DuplicateTable(
	tabID TableID, remapColumnIDs func(e ScalarExpr, colMap ColMap) ScalarExpr,
) TableID {
	if md.tables == nil || tabID.index() >= len(md.tables) {
		panic(errors.AssertionFailedf("table with ID %d does not exist", tabID))
	}

	tabMeta := md.TableMeta(tabID)
	tab := tabMeta.Table
	newTabID := makeTableID(len(md.tables), ColumnID(len(md.cols)+1))

	// Generate new column IDs for each column in the table, and keep track of
	// a mapping from the original TableMeta's column IDs to the new ones.
	var colMap ColMap
	for i, n := 0, tab.ColumnCount(); i < n; i++ {
		col := tab.Column(i)
		oldColID := tabID.ColumnID(i)
		newColID := md.AddColumn(string(col.ColName()), col.DatumType())
		md.ColumnMeta(newColID).Table = newTabID
		colMap.Set(int(oldColID), int(newColID))
	}

	// Create new constraints by remapping the column IDs to the new TableMeta's
	// column IDs.
	var constraints ScalarExpr
	if tabMeta.Constraints != nil {
		constraints = remapColumnIDs(tabMeta.Constraints, colMap)
	}

	// Create new computed column expressions by remapping the column IDs in
	// each ScalarExpr.
	var computedCols map[ColumnID]ScalarExpr
	var referencedColsInComputedExpressions ColSet
	if len(tabMeta.ComputedCols) > 0 {
		computedCols = make(map[ColumnID]ScalarExpr, len(tabMeta.ComputedCols))
		for colID, e := range tabMeta.ComputedCols {
			newColID, ok := colMap.Get(int(colID))
			if !ok {
				panic(errors.AssertionFailedf("column with ID %d does not exist in map", colID))
			}
			computedCols[ColumnID(newColID)] = remapColumnIDs(e, colMap)
		}
		// Add columns present in newScalarExpr to referencedColsInComputedExpressions.
		referencedColsInComputedExpressions =
			tabMeta.ColsInComputedColsExpressions.CopyAndMaybeRemap(colMap)
	}

	// Create new partial index predicate expressions by remapping the column
	// IDs in each ScalarExpr.
	var partialIndexPredicates map[cat.IndexOrdinal]ScalarExpr
	if len(tabMeta.partialIndexPredicates) > 0 {
		partialIndexPredicates = make(map[cat.IndexOrdinal]ScalarExpr, len(tabMeta.partialIndexPredicates))
		for idxOrd, e := range tabMeta.partialIndexPredicates {
			partialIndexPredicates[idxOrd] = remapColumnIDs(e, colMap)
		}
	}

	var checkConstraintsStats map[ColumnID]interface{}
	if len(tabMeta.checkConstraintsStats) > 0 {
		checkConstraintsStats =
			make(map[ColumnID]interface{},
				len(tabMeta.checkConstraintsStats))
		for i := range tabMeta.checkConstraintsStats {
			if dstCol, ok := colMap.Get(int(i)); ok {
				// We remap the column ID key, but not any column IDs in the
				// ColumnStatistic as this is still being used in the statistics of the
				// original table and should be treated as immutable. When the Histogram
				// is copied in ColumnStatistic.CopyFromOther, it is initialized with
				// the proper column ID.
				checkConstraintsStats[ColumnID(dstCol)] = tabMeta.checkConstraintsStats[i]
			} else {
				panic(errors.AssertionFailedf("remapping of check constraint stats column failed"))
			}
		}
	}

	newTabMeta := TableMeta{
		MetaID:                        newTabID,
		Table:                         tabMeta.Table,
		Alias:                         tabMeta.Alias,
		IgnoreForeignKeys:             tabMeta.IgnoreForeignKeys,
		Constraints:                   constraints,
		ComputedCols:                  computedCols,
		ColsInComputedColsExpressions: referencedColsInComputedExpressions,
		partialIndexPredicates:        partialIndexPredicates,
		indexPartitionLocalities:      tabMeta.indexPartitionLocalities,
		checkConstraintsStats:         checkConstraintsStats,
		notVisibleIndexMap:            tabMeta.notVisibleIndexMap,
	}
	md.tables = append(md.tables, newTabMeta)
	regionConfig, ok := md.TableAnnotation(tabID, regionConfigAnnID).(*multiregion.RegionConfig)
	if ok {
		// Don't waste time looking up a database descriptor and constructing a
		// RegionConfig more than once for a given table.
		md.SetTableAnnotation(newTabID, regionConfigAnnID, regionConfig)
	}

	return newTabID
}

// TableMeta looks up the metadata for the table associated with the given table
// id. The same table can be added multiple times to the query metadata and
// associated with multiple table ids.
func (md *Metadata) TableMeta(tabID TableID) *TableMeta {
	return &md.tables[tabID.index()]
}

// Table looks up the catalog table associated with the given metadata id. The
// same table can be associated with multiple metadata ids.
func (md *Metadata) Table(tabID TableID) cat.Table {
	return md.TableMeta(tabID).Table
}

// AllTables returns the metadata for all tables. The result must not be
// modified.
func (md *Metadata) AllTables() []TableMeta {
	return md.tables
}

// AddColumn assigns a new unique id to a column within the query and records
// its alias and type. If the alias is empty, a "column<ID>" alias is created.
func (md *Metadata) AddColumn(alias string, typ *types.T) ColumnID {
	if alias == "" {
		alias = fmt.Sprintf("column%d", len(md.cols)+1)
	}
	colID := ColumnID(len(md.cols) + 1)
	md.cols = append(md.cols, ColumnMeta{MetaID: colID, Alias: alias, Type: typ})
	return colID
}

// NumColumns returns the count of columns tracked by this Metadata instance.
func (md *Metadata) NumColumns() int {
	return len(md.cols)
}

// ColumnMeta looks up the metadata for the column associated with the given
// column id. The same column can be added multiple times to the query metadata
// and associated with multiple column ids.
func (md *Metadata) ColumnMeta(colID ColumnID) *ColumnMeta {
	return &md.cols[colID.index()]
}

// QualifiedAlias returns the column alias, possibly qualified with the table,
// schema, or database name:
//
//  1. If fullyQualify is true, then the returned alias is prefixed by the
//     original, fully qualified name of the table: tab.Name().FQString().
//
//  2. If there's another column in the metadata with the same column alias but
//     a different table name, then prefix the column alias with the table
//     name: "tabName.columnAlias". If alwaysQualify is true, then the column
//     alias is always prefixed with the table alias.
func (md *Metadata) QualifiedAlias(
	colID ColumnID, fullyQualify, alwaysQualify bool, catalog cat.Catalog,
) string {
	cm := md.ColumnMeta(colID)
	if cm.Table == 0 {
		// Column doesn't belong to a table, so no need to qualify it further.
		return cm.Alias
	}

	// If a fully qualified alias has not been requested, then only qualify it if
	// it would otherwise be ambiguous.
	var tabAlias tree.TableName
	qualify := fullyQualify || alwaysQualify
	if !fullyQualify {
		tabAlias = md.TableMeta(cm.Table).Alias
		for i := range md.cols {
			if i == int(cm.MetaID-1) {
				continue
			}

			// If there are two columns with same alias, then column is ambiguous.
			cm2 := &md.cols[i]
			if cm2.Alias == cm.Alias {
				if cm2.Table == 0 {
					qualify = true
				} else {
					// Only qualify if the qualified names are actually different.
					tabAlias2 := md.TableMeta(cm2.Table).Alias
					if tabAlias.String() != tabAlias2.String() {
						qualify = true
					}
				}
			}
		}
	}

	// If the column name should not even be partly qualified, then no more to do.
	if !qualify {
		return cm.Alias
	}

	var sb strings.Builder
	if fullyQualify {
		tn, err := catalog.FullyQualifiedName(context.TODO(), md.TableMeta(cm.Table).Table)
		if err != nil {
			panic(err)
		}
		sb.WriteString(tn.FQString())
	} else {
		sb.WriteString(tabAlias.String())
	}
	sb.WriteRune('.')
	sb.WriteString(cm.Alias)
	return sb.String()
}

// UpdateTableMeta allows the caller to replace the cat.Table struct that a
// TableMeta instance stores.
func (md *Metadata) UpdateTableMeta(evalCtx *eval.Context, tables map[cat.StableID]cat.Table) {
	for i := range md.tables {
		oldTable := md.tables[i].Table
		if newTable, ok := tables[oldTable.ID()]; ok {
			// If there are any inverted hypothetical indexes, the hypothetical table
			// will have extra inverted columns added. Add any new inverted columns to
			// the metadata.
			for j, n := oldTable.ColumnCount(), newTable.ColumnCount(); j < n; j++ {
				md.AddColumn(string(newTable.Column(j).ColName()), types.Bytes)
			}
			if newTable.ColumnCount() > oldTable.ColumnCount() {
				// If we added any new columns, we need to recalculate the not null
				// column set.
				md.SetTableAnnotation(md.tables[i].MetaID, NotNullAnnID, nil)
			}
			md.tables[i].Table = newTable
			md.tables[i].CacheIndexPartitionLocalities(evalCtx)
		}
	}
}

// SequenceID uniquely identifies the usage of a sequence within the scope of a
// query. SequenceID 0 is reserved to mean "unknown sequence".
type SequenceID uint64

// index returns the index of the sequence in Metadata.sequences. It's biased by 1, so
// that SequenceID 0 can be be reserved to mean "unknown sequence".
func (s SequenceID) index() int {
	return int(s - 1)
}

// makeSequenceID constructs a new SequenceID from its component parts.
func makeSequenceID(index int) SequenceID {
	// Bias the sequence index by 1.
	return SequenceID(index + 1)
}

// AddSequence adds the sequence to the metadata, returning a SequenceID that
// can be used to retrieve it.
func (md *Metadata) AddSequence(seq cat.Sequence) SequenceID {
	seqID := makeSequenceID(len(md.sequences))
	if md.sequences == nil {
		md.sequences = make([]cat.Sequence, 0, 4)
	}
	md.sequences = append(md.sequences, seq)

	return seqID
}

// Sequence looks up the catalog sequence associated with the given metadata id. The
// same sequence can be associated with multiple metadata ids.
func (md *Metadata) Sequence(seqID SequenceID) cat.Sequence {
	return md.sequences[seqID.index()]
}

// UniqueID should be used to disambiguate multiple uses of an expression
// within the scope of a query. For example, a UniqueID field should be
// added to an expression type if two instances of that type might otherwise
// be indistinguishable based on the values of their other fields.
//
// See the comment for Metadata for more details on identifiers.
type UniqueID uint64

// NextUniqueID returns a fresh UniqueID which is guaranteed to never have been
// previously allocated in this memo.
func (md *Metadata) NextUniqueID() UniqueID {
	md.currUniqueID++
	return md.currUniqueID
}

// AddView adds a new reference to a view used by the query.
func (md *Metadata) AddView(v cat.View) {
	md.views = append(md.views, v)
}

// AllViews returns the metadata for all views. The result must not be
// modified.
func (md *Metadata) AllViews() []cat.View {
	return md.views
}

// getAllReferencedTables returns all the tables referenced by the metadata.
// This includes all tables that are directly stored in the metadata in
// md.tables, as well as recursive references from foreign keys. The tables are
// returned in sorted order so that later tables reference earlier tables. This
// allows tables to be re-created in order (e.g., for statement-bundle recreate)
// using the output from SHOW CREATE TABLE without any errors due to missing
// tables.
// TODO(rytaft): if there is a cycle in the foreign key references,
// statement-bundle recreate will still hit errors. To handle this case, we
// would need to first create the tables without the foreign keys, then add the
// foreign keys later.
func (md *Metadata) getAllReferencedTables(
	ctx context.Context, catalog cat.Catalog,
) []cat.DataSource {
	var tableSet intsets.Fast
	var tableList []cat.DataSource
	var addForeignKeyReferencedTables func(tab cat.Table)
	addForeignKeyReferencedTables = func(tab cat.Table) {
		for i := 0; i < tab.OutboundForeignKeyCount(); i++ {
			tabID := tab.OutboundForeignKey(i).ReferencedTableID()
			if !tableSet.Contains(int(tabID)) {
				tableSet.Add(int(tabID))
				ds, _, err := catalog.ResolveDataSourceByID(ctx, cat.Flags{}, tabID)
				if err != nil {
					// This is a best-effort attempt to get all the tables, so don't error.
					continue
				}
				refTab, ok := ds.(cat.Table)
				if !ok {
					// This is a best-effort attempt to get all the tables, so don't error.
					continue
				}
				addForeignKeyReferencedTables(refTab)
				tableList = append(tableList, ds)
			}
		}
	}
	for i := range md.tables {
		tabMeta := md.tables[i]
		tabID := tabMeta.Table.ID()
		if !tableSet.Contains(int(tabID)) {
			tableSet.Add(int(tabID))
			addForeignKeyReferencedTables(tabMeta.Table)
			tableList = append(tableList, tabMeta.Table)
		}
	}
	return tableList
}

// AllDataSourceNames returns the fully qualified names of all datasources
// referenced by the metadata. This includes all tables, sequences, and views
// that are directly stored in the metadata, as well as tables that are
// recursively referenced from foreign keys.
func (md *Metadata) AllDataSourceNames(
	ctx context.Context,
	catalog cat.Catalog,
	fullyQualifiedName func(ds cat.DataSource) (cat.DataSourceName, error),
) (tables, sequences, views []tree.TableName, _ error) {
	// Catalog objects can show up multiple times in our lists, so deduplicate
	// them.
	seen := make(map[tree.TableName]struct{})

	getNames := func(count int, get func(int) cat.DataSource) ([]tree.TableName, error) {
		result := make([]tree.TableName, 0, count)
		for i := 0; i < count; i++ {
			ds := get(i)
			tn, err := fullyQualifiedName(ds)
			if err != nil {
				return nil, err
			}
			if _, ok := seen[tn]; !ok {
				seen[tn] = struct{}{}
				result = append(result, tn)
			}
		}
		return result, nil
	}
	var err error
	refTables := md.getAllReferencedTables(ctx, catalog)
	tables, err = getNames(len(refTables), func(i int) cat.DataSource {
		return refTables[i]
	})
	if err != nil {
		return nil, nil, nil, err
	}
	sequences, err = getNames(len(md.sequences), func(i int) cat.DataSource {
		return md.sequences[i]
	})
	if err != nil {
		return nil, nil, nil, err
	}
	views, err = getNames(len(md.views), func(i int) cat.DataSource {
		return md.views[i]
	})
	if err != nil {
		return nil, nil, nil, err
	}
	return tables, sequences, views, nil
}

// WithID uniquely identifies a With expression within the scope of a query.
// WithID=0 is reserved to mean "unknown expression".
// See the comment for Metadata for more details on identifiers.
type WithID uint64

// AddWithBinding associates a WithID to its bound expression.
func (md *Metadata) AddWithBinding(id WithID, expr Expr) {
	if md.withBindings == nil {
		md.withBindings = make(map[WithID]Expr)
	}
	md.withBindings[id] = expr
}

// WithBinding returns the bound expression for the given WithID.
// Panics with an assertion error if there is none.
func (md *Metadata) WithBinding(id WithID) Expr {
	res, ok := md.withBindings[id]
	if !ok {
		panic(errors.AssertionFailedf("no binding for WithID %d", id))
	}
	return res
}

// ForEachWithBinding calls fn with each bound (WithID, Expr) pair in the
// metadata.
func (md *Metadata) ForEachWithBinding(fn func(WithID, Expr)) {
	for id, expr := range md.withBindings {
		fn(id, expr)
	}
}

// TestingDataSourceDeps exposes the dataSourceDeps for testing.
func (md *Metadata) TestingDataSourceDeps() map[cat.StableID]cat.DataSource {
	return md.dataSourceDeps
}

// TestingUDFDeps exposes the udfDeps for testing.
func (md *Metadata) TestingUDFDeps() map[cat.StableID]*tree.Overload {
	return md.udfDeps
}

// TestingObjectRefsByName exposes the objectRefsByName for testing.
func (md *Metadata) TestingObjectRefsByName() map[cat.StableID][]*tree.UnresolvedObjectName {
	return md.objectRefsByName
}

// TestingPrivileges exposes the privileges for testing.
func (md *Metadata) TestingPrivileges() map[cat.StableID]privilegeBitmap {
	return md.privileges
}
