// Copyright 2016 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package sql

import (
	"context"
	"fmt"
	"strings"

	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/server/telemetry"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/dbdesc"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/multiregion"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/schemadesc"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/tabledesc"
	"github.com/cockroachdb/cockroach/pkg/sql/decodeusername"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgnotice"
	"github.com/cockroachdb/cockroach/pkg/sql/regions"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/catconstants"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/eval"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlerrors"
	"github.com/cockroachdb/cockroach/pkg/sql/sqltelemetry"
	"github.com/cockroachdb/cockroach/pkg/util/uuid"
	"github.com/cockroachdb/errors"
)

//
// This file contains routines for low-level access to stored
// descriptors.
//
// For higher levels in the SQL layer, these interface are likely not
// suitable; consider instead schema_accessors.go and resolver.go.
//

var (
	errEmptyDatabaseName = pgerror.New(pgcode.Syntax, "empty database name")
	errNoDatabase        = pgerror.New(pgcode.InvalidName, "no database specified")
	errNoSchema          = pgerror.Newf(pgcode.InvalidName, "no schema specified")
	errNoTable           = pgerror.New(pgcode.InvalidName, "no table specified")
	errNoType            = pgerror.New(pgcode.InvalidName, "no type specified")
	errNoFunction        = pgerror.New(pgcode.InvalidName, "no function specified")
	errNoMatch           = pgerror.New(pgcode.UndefinedObject, "no object matched")
)

// PublicSchemaCreatePrivilegeEnabled is the cluster setting that determines
// whether the CREATE privilege is given to the `public` role on the `public`
// schema at the time the schema is created.
var PublicSchemaCreatePrivilegeEnabled = settings.RegisterBoolSetting(
	settings.TenantWritable,
	"sql.auth.public_schema_create_privilege.enabled",
	"determines whether to grant all users the CREATE privileges on the public "+
		"schema when it is created",
	true,
	settings.WithPublic)

// createDatabase takes Database descriptor and creates it if needed,
// incrementing the descriptor counter. Returns true if the descriptor
// is actually created, false if it already existed, or an error if one was
// encountered. The ifNotExists flag is used to declare if the "already existed"
// state should be an error (false) or a no-op (true).
// createDatabase implements the DatabaseDescEditor interface.
func (p *planner) createDatabase(
	ctx context.Context, database *tree.CreateDatabase, jobDesc string,
) (*dbdesc.Mutable, bool, error) {

	dbName := string(database.Name)

	if dbID, err := p.Descriptors().LookupDatabaseID(ctx, p.txn, dbName); err == nil && dbID != descpb.InvalidID {
		if database.IfNotExists {
			// Check if the database is in a dropping state
			desc, err := p.Descriptors().ByID(p.txn).Get().Database(ctx, dbID)
			if err != nil {
				return nil, false, err
			}
			if desc.Dropped() {
				return nil, false, pgerror.Newf(pgcode.ObjectNotInPrerequisiteState,
					"database %q is being dropped, try again later",
					dbName)
			}
			// Noop.
			return nil, false, nil
		}
		return nil, false, sqlerrors.NewDatabaseAlreadyExistsError(dbName)
	} else if err != nil {
		return nil, false, err
	}

	id, err := p.extendedEvalCtx.DescIDGenerator.GenerateUniqueDescID(ctx)
	if err != nil {
		return nil, false, err
	}
	publicSchemaID, err := p.EvalContext().DescIDGenerator.GenerateUniqueDescID(ctx)
	if err != nil {
		return nil, false, err
	}

	if database.PrimaryRegion != tree.PrimaryRegionNotSpecifiedName {
		telemetry.Inc(sqltelemetry.CreateMultiRegionDatabaseCounter)
		telemetry.Inc(
			sqltelemetry.CreateDatabaseSurvivalGoalCounter(
				database.SurvivalGoal.TelemetryName(),
			),
		)
		if database.Placement != tree.DataPlacementUnspecified {
			telemetry.Inc(
				sqltelemetry.CreateDatabasePlacementCounter(
					database.Placement.TelemetryName(),
				),
			)
		}
	}

	regionConfig, err := p.maybeInitializeMultiRegionMetadata(
		ctx,
		database.SurvivalGoal,
		database.PrimaryRegion,
		database.Regions,
		database.Placement,
		database.SecondaryRegion,
	)
	if err != nil {
		return nil, false, err
	}

	owner := p.SessionData().User()
	if !database.Owner.Undefined() {
		owner, err = decodeusername.FromRoleSpec(
			p.SessionData(), username.PurposeValidation, database.Owner,
		)
		if err != nil {
			return nil, true, err
		}
	}

	db := dbdesc.NewInitial(
		id,
		string(database.Name),
		owner,
		dbdesc.MaybeWithDatabaseRegionConfig(regionConfig),
		dbdesc.WithPublicSchemaID(publicSchemaID),
	)
	includeCreatePriv := PublicSchemaCreatePrivilegeEnabled.Get(&p.execCfg.Settings.SV)
	publicSchema := schemadesc.NewBuilder(&descpb.SchemaDescriptor{
		ParentID:   id,
		Name:       catconstants.PublicSchemaName,
		ID:         publicSchemaID,
		Privileges: catpb.NewPublicSchemaPrivilegeDescriptor(includeCreatePriv),
		Version:    1,
	}).BuildCreatedMutableSchema()

	if err := p.checkCanAlterToNewOwner(ctx, db, owner); err != nil {
		return nil, true, err
	}

	if err := p.createDescriptor(ctx, db, jobDesc); err != nil {
		return nil, true, err
	}
	if err := p.createDescriptor(
		ctx,
		publicSchema,
		tree.AsStringWithFQNames(database, p.Ann()),
	); err != nil {
		return nil, true, err
	}

	// Initialize the multi-region database by creating the multi-region enum and
	// database-level zone configuration if there is a region config on the
	// descriptor.

	if err := p.maybeInitializeMultiRegionDatabase(ctx, db, regionConfig); err != nil {
		return nil, true, err
	}

	if database.SuperRegion.Name != "" {
		if err := p.isSuperRegionEnabled(); err != nil {
			return nil, false, err
		}

		typeID, err := db.MultiRegionEnumID()
		if err != nil {
			return nil, false, err
		}
		typeDesc, err := p.Descriptors().MutableByID(p.txn).Type(ctx, typeID)
		if err != nil {
			return nil, false, err
		}

		if err = p.addSuperRegion(
			ctx,
			db,
			typeDesc,
			database.SuperRegion.Regions,
			database.SuperRegion.Name,
			tree.AsStringWithFQNames(database, p.Ann()),
		); err != nil {
			return nil, false, err
		}

	}

	// TODO(jeffswenson): delete once region_livess is implemented (#107966)
	if err := p.maybeUpdateSystemDBSurvivalGoal(ctx); err != nil {
		return nil, false, err
	}

	return db, true, nil
}

func (p *planner) createDescriptor(
	ctx context.Context, descriptor catalog.MutableDescriptor, jobDesc string,
) error {
	if !descriptor.IsNew() {
		return errors.AssertionFailedf(
			"expected new descriptor, not a modification of version %d",
			descriptor.OriginalVersion())
	}
	b := p.Txn().NewBatch()
	kvTrace := p.ExtendedEvalContext().Tracing.KVTracingEnabled()
	if err := p.Descriptors().WriteDescToBatch(ctx, kvTrace, descriptor, b); err != nil {
		return err
	}
	if !descriptor.SkipNamespace() {
		if err := p.Descriptors().InsertNamespaceEntryToBatch(ctx, kvTrace, descriptor, b); err != nil {
			return err
		}
	}
	if err := p.txn.Run(ctx, b); err != nil {
		return err
	}
	if tbl, ok := descriptor.(*tabledesc.Mutable); ok && tbl.Adding() {
		// Queue a schema change job to eventually make the table public.
		if err := p.createOrUpdateSchemaChangeJob(
			ctx, tbl, jobDesc, descpb.InvalidMutationID,
		); err != nil {
			return err
		}
	}
	return nil
}

// TranslateSurvivalGoal translates a tree.SurvivalGoal into a
// descpb.SurvivalGoal.
func TranslateSurvivalGoal(g tree.SurvivalGoal) (descpb.SurvivalGoal, error) {
	switch g {
	case tree.SurvivalGoalDefault:
		return descpb.SurvivalGoal_ZONE_FAILURE, nil
	case tree.SurvivalGoalZoneFailure:
		return descpb.SurvivalGoal_ZONE_FAILURE, nil
	case tree.SurvivalGoalRegionFailure:
		return descpb.SurvivalGoal_REGION_FAILURE, nil
	default:
		return 0, errors.Newf("unknown survival goal: %d", g)
	}
}

// TranslateProtoSurvivalGoal translate a descpb.SurvivalGoal into a
// tree.SurvivalGoal.
func TranslateProtoSurvivalGoal(g descpb.SurvivalGoal) (tree.SurvivalGoal, error) {
	switch g {
	case descpb.SurvivalGoal_ZONE_FAILURE:
		return tree.SurvivalGoalZoneFailure, nil
	case descpb.SurvivalGoal_REGION_FAILURE:
		return tree.SurvivalGoalRegionFailure, nil
	default:
		return 0, errors.Newf("unknown survival goal: %d", g)
	}
}

// TranslateDataPlacement translates a tree.DataPlacement into a
// descpb.DataPlacement.
func TranslateDataPlacement(g tree.DataPlacement) (descpb.DataPlacement, error) {
	switch g {
	case tree.DataPlacementUnspecified:
		return descpb.DataPlacement_DEFAULT, nil
	case tree.DataPlacementDefault:
		return descpb.DataPlacement_DEFAULT, nil
	case tree.DataPlacementRestricted:
		return descpb.DataPlacement_RESTRICTED, nil
	default:
		return 0, errors.AssertionFailedf("unknown data placement: %d", g)
	}
}

// checkRegionIsCurrentlyActive looks up the set of active regions and
// checks whether the region argument is in that set. If this is a secondary
// tenant, and this check is for the system database, we augment the behavior
// to only look up the host regions.
func (p *planner) checkRegionIsCurrentlyActive(
	ctx context.Context, region catpb.RegionName, isSystemDatabase bool,
) error {
	var liveRegions LiveClusterRegions
	if !p.execCfg.Codec.ForSystemTenant() && isSystemDatabase {
		systemRegions, err := p.regionsProvider().GetSystemRegions(ctx)
		if err != nil {
			return err
		}
		liveRegions = regionsResponseToLiveClusterRegions(systemRegions)
	} else {
		var err error
		if liveRegions, err = p.getLiveClusterRegions(ctx); err != nil {
			return err
		}
	}

	// Ensure that the region we're adding is currently active.
	return CheckClusterRegionIsLive(liveRegions, region)
}

// InitializeMultiRegionMetadataCCL is the public hook point for the
// CCL-licensed multi-region initialization code.
var InitializeMultiRegionMetadataCCL = func(
	ctx context.Context,
	descIDGenerator eval.DescIDGenerator,
	settings *cluster.Settings,
	clusterID uuid.UUID,
	liveClusterRegions LiveClusterRegions,
	survivalGoal tree.SurvivalGoal,
	primaryRegion catpb.RegionName,
	regions []tree.Name,
	dataPlacement tree.DataPlacement,
	secondaryRegion catpb.RegionName,
) (*multiregion.RegionConfig, error) {
	return nil, sqlerrors.NewCCLRequiredError(
		errors.New("creating multi-region databases requires a CCL binary"),
	)
}

// DefaultPrimaryRegionClusterSettingName is the name of the cluster setting that returns
const DefaultPrimaryRegionClusterSettingName = "sql.defaults.primary_region"

// DefaultPrimaryRegion is a cluster setting that contains the default primary region.
var DefaultPrimaryRegion = settings.RegisterStringSetting(
	settings.TenantWritable,
	DefaultPrimaryRegionClusterSettingName,
	`if not empty, all databases created without a PRIMARY REGION will `+
		`implicitly have the given PRIMARY REGION`,
	"",
	settings.WithPublic)

// SecondaryTenantsMultiRegionAbstractionsEnabledSettingName is the name of the
// cluster setting that governs secondary tenant multi-region abstraction usage.
const SecondaryTenantsMultiRegionAbstractionsEnabledSettingName = "sql.virtual_cluster.feature_access.multiregion.enabled"

// SecondaryTenantsMultiRegionAbstractionsEnabled controls if secondary tenants
// are allowed to use multi-region abstractions. In particular, it controls if
// secondary tenants are allowed to add a region to their database. It has no
// effect on the system tenant.
//
// This setting has no effect for existing multi-region databases that have
// already been configured. It only affects regions being added to new
// databases.
var SecondaryTenantsMultiRegionAbstractionsEnabled = settings.RegisterBoolSetting(
	settings.TenantReadOnly,
	"sql.multi_region.allow_abstractions_for_secondary_tenants.enabled", // internal key, name defined above
	"allow the use of multi-region abstractions and syntax in virtual clusters",
	false,
	settings.WithName(SecondaryTenantsMultiRegionAbstractionsEnabledSettingName),
)

// maybeInitializeMultiRegionMetadata initializes multi-region metadata if a
// primary region is supplied and works as a pass-through otherwise. It creates
// a new region config from the given parameters and reserves an ID for the
// multi-region enum.
func (p *planner) maybeInitializeMultiRegionMetadata(
	ctx context.Context,
	survivalGoal tree.SurvivalGoal,
	primaryRegion tree.Name,
	regions []tree.Name,
	placement tree.DataPlacement,
	secondaryRegion tree.Name,
) (*multiregion.RegionConfig, error) {
	if !p.execCfg.Codec.ForSystemTenant() &&
		!SecondaryTenantsMultiRegionAbstractionsEnabled.Get(&p.execCfg.Settings.SV) {
		// There was no primary region provided, let the thing pass through.
		if primaryRegion == "" && len(regions) == 0 {
			return nil, nil
		}

		return nil, errors.WithHint(pgerror.Newf(
			pgcode.InvalidDatabaseDefinition,
			"setting %s disallows use of multi-region abstractions",
			SecondaryTenantsMultiRegionAbstractionsEnabledSettingName,
		),
			"consider omitting the primary region")
	}

	if primaryRegion == "" && len(regions) == 0 {
		var err error
		primaryRegion, regions, err = p.getDefaultDatabaseRegions(ctx)
		if err != nil {
			return nil, err
		}
		if primaryRegion == "" {
			return nil, nil
		}
		p.BufferClientNotice(ctx, formatDefaultRegionNotice(primaryRegion, regions))
	}

	liveRegions, err := p.getLiveClusterRegions(ctx)
	if err != nil {
		return nil, err
	}

	regionConfig, err := InitializeMultiRegionMetadataCCL(
		ctx,
		p.EvalContext().DescIDGenerator,
		p.EvalContext().Settings,
		p.ExecCfg().NodeInfo.LogicalClusterID(),
		liveRegions,
		survivalGoal,
		catpb.RegionName(primaryRegion),
		regions,
		placement,
		catpb.RegionName(secondaryRegion),
	)
	if err != nil {
		return nil, err
	}

	return regionConfig, nil
}

// formatDefaultRegionNotice formats an error that looks like:
//
// defaulting to 'WITH PRIMARY REGION "us-east1" REGIONS "us-west2",
// "us-central3"' as no primary region was specified
//
// The error message is intended to echo sql that is equivalent to the default
// behavior.
func formatDefaultRegionNotice(primary tree.Name, regions []tree.Name) pgnotice.Notice {
	var message strings.Builder
	fmt.Fprintf(&message, `defaulting to 'WITH PRIMARY REGION "%s"`, primary)

	// Add non-primary regions to the message if present.
	if 0 < len(regions) {
		fmt.Fprintf(&message, ` REGIONS`)
		for i := range regions {
			fmt.Fprintf(&message, ` "%s"`, regions[i])
			if i < len(regions)-1 {
				fmt.Fprint(&message, `,`)
			}
		}
	}

	fmt.Fprint(&message, `' as no primary region was specified`)
	return pgnotice.Newf("%s", message.String())
}

// getDefaultDatabaseRegions returns the default primary and nonPrimary regions
// for a database if the user did not specify a primary region via sql.
func (p *planner) getDefaultDatabaseRegions(
	ctx context.Context,
) (primary tree.Name, nonPrimary []tree.Name, err error) {
	// If 'sql.defaults.primary_region' is set, use the setting value.
	defaultPrimaryRegion := DefaultPrimaryRegion.Get(&p.execCfg.Settings.SV)
	if defaultPrimaryRegion != "" {
		return tree.Name(defaultPrimaryRegion), nil, nil
	}

	// Otherwise, retrieve the primary region from the system database's
	// descriptor and the set of all regions from the system database's region
	// enum.
	systemDatabase, err := p.Descriptors().ByIDWithLeased(p.txn).Get().Database(ctx, keys.SystemDatabaseID)
	if err != nil {
		return "", nil, errors.NewAssertionErrorWithWrappedErrf(
			err, "failed to resolve system database for regions",
		)
	}

	enumRegions, err := regions.GetDatabaseRegions(ctx, p.txn, systemDatabase, p.Descriptors())
	if err != nil {
		return "", nil, err
	}
	if len(enumRegions) == 0 {
		return "", nil, nil
	}

	primaryRegion := tree.Name(systemDatabase.GetRegionConfig().PrimaryRegion)
	for region := range enumRegions {
		nextRegion := tree.Name(region)
		if nextRegion != primaryRegion {
			nonPrimary = append(nonPrimary, nextRegion)
		}
	}

	return primaryRegion, nonPrimary, nil
}

// GetImmutableTableInterfaceByID is part of the EvalPlanner interface.
func (p *planner) GetImmutableTableInterfaceByID(ctx context.Context, id int) (interface{}, error) {
	desc, err := p.Descriptors().ByIDWithLeased(p.txn).WithoutNonPublic().Get().Table(ctx, descpb.ID(id))
	if err != nil {
		return nil, err
	}
	return desc, nil
}
