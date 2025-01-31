load("@io_bazel_rules_go//go:def.bzl", "go_context")

# _parse_migrations takes a list of golang-migrate compatible schema files (in
# the format <timestamp>_some_description_{up,down}.sql) and splits them into
# 'up' and 'down' dictionaries, each a map from timestamp to underlying file.
#
# It also does some checks on the provided file names, making sure that
# golang-migrate will parse them correctly.
def _parse_migrations(files):
    uppers = {}
    downers = {}

    # Ensure filename fits golang-migrate format, sort into 'up' and 'down' files.
    for file in files:
        if not file.basename.endswith(".up.sql") and not file.basename.endswith(".down.sql"):
            fail("migration %s must end woth .{up,down}.sql" % file.basename)
        if len(file.basename.split('.')) != 3:
            fail("migration %s must not contain any . other than in .{up,down}.sql extension" % file.basename)
        first = file.basename.split('.')[0]
        if len(first.split('_')) < 2:
            fail("migration %s must be in <timestamp>_<name>.{up,down}.sql format" % file.basename)
        timestamp = first.split('_')[0]
        if not timestamp.isdigit():
            fail("migration %s must be in <timestamp>_<name>.{up,down}.sql format" % file.basename)
        timestamp = int(timestamp)
        if timestamp < 1662136250:
            fail("migration %s must be in <timestamp>_<name>.{up,down}.sql format" % file.basename)

        if file.basename.endswith('.up.sql'):
            if timestamp in uppers:
               fail("migration %s conflicts with %s" % [file.basename, uppers[timestamp].basename])
            uppers[timestamp] = file
        if file.basename.endswith('.down.sql'):
            if timestamp in downers:
               fail("migration %s conflicts with %s" % [file.basename, downers[timestamp].basename])
            downers[timestamp] = file

    # Check each 'up' has a corresponding 'down', and vice-versa.
    for timestamp, up in uppers.items():
        if timestamp not in downers:
            fail("%s has no corresponding 'down' migration" % up.basename)
        if downers[timestamp].basename.replace('down.sql', 'up.sql') != up.basename:
            fail("%s has no corresponding 'down' migration" % up.basename)
    for timestamp, down in downers.items():
        if timestamp not in uppers:
            fail("%s has no corresponding 'up' migration" % down.basename)
        if uppers[timestamp].basename.replace('up.sql', 'down.sql') != down.basename:
            fail("%s has no corresponding 'up' migration" % down.basename)

    return uppers, downers

def _sqlc_go_library(ctx):
    go = go_context(ctx)

    importpath_parts = ctx.attr.importpath.split("/")
    package_name = importpath_parts[-1]

    # Split migrations into 'up' and 'down'. Only pass 'up' to sqlc. Use both
    # to generate golang-migrate compatible bindata.
    uppers, downers = _parse_migrations(ctx.files.migrations)

    # Make sure given queries have no repeating basenames. This ensures clean
    # mapping source SQL file name and generated Go file.
    query_basenames = []
    for query in ctx.files.queries:
        if query.basename in query_basenames:
            fail("duplicate %s base name in query files" % query.basename)
        query_basenames.append(query.basename)

    # Go files generated by sqlc.
    sqlc_go_sources = [
        # db.go and models.go always exist.
        ctx.actions.declare_file("db.go"),
        ctx.actions.declare_file("models.go"),
    ]
    # For every query file, basename.go is also generated.
    for basename in query_basenames:
        sqlc_go_sources.append(ctx.actions.declare_file(basename + ".go"))

    # Cockroachdb is PostgreSQL with some extra overrides to fix Go/SQL type
    # mappings.
    overrides = []
    if ctx.attr.dialect == "cockroachdb":
        overrides = [
            # INT is 64-bit in cockroachdb (32-bit in postgres).
            { "go_type": "int64", "db_type": "pg_catalog.int4" },
        ]

    config = ctx.actions.declare_file("_config.yaml")
    # All paths in config are relative to the config file. However, Bazel paths
    # are relative to the execution root/CWD. To make things work regardless of
    # config file placement, we prepend all config paths with a `../../ ...`
    # path walk that makes the path be execroot relative again.
    config_walk = '../' * config.path.count('/')
    config_data = json.encode({
        "version": 2,
        "sql": [
            {
                "schema": [config_walk + up.path for up in uppers.values()],
                "queries": [config_walk + query.path for query in ctx.files.queries],
                "engine": "postgresql",
                "gen": {
                    "go": {
                        "package": package_name,
                        "out": config_walk + sqlc_go_sources[0].dirname,
                        "overrides": overrides,
                    },
                },
            },
        ],
    })
    ctx.actions.write(config, config_data)

    # Generate types/functions using sqlc.
    ctx.actions.run(
        mnemonic = "SqlcGen",
        executable = ctx.executable._sqlc,
        arguments = [
            "generate",
            "-f", config.path,
        ],
        inputs = [
            config
        ] + uppers.values() + ctx.files.queries,
        outputs = sqlc_go_sources,
    )

    library = go.new_library(go, srcs = sqlc_go_sources, importparth = ctx.attr.importpath)
    source = go.library_to_source(go, ctx.attr, library, ctx.coverage_instrumented())
    return [
        library,
        source,
        OutputGroupInfo(go_generated_srcs = depset(library.srcs)),
    ]


sqlc_go_library = rule(
    implementation = _sqlc_go_library,
    attrs = {
        "migrations": attr.label_list(
            allow_files = True,
        ),
        "queries": attr.label_list(
            allow_files = True,
        ),
        "importpath": attr.string(
            mandatory = True,
        ),
        "dialect": attr.string(
            mandatory = True,
            values = ["postgresql", "cockroachdb"],
        ),
        "_sqlc": attr.label(
            default = Label("@com_github_kyleconroy_sqlc//cmd/sqlc"),
            allow_single_file = True,
            executable = True,
            cfg = "exec",
        ),
        "_bindata": attr.label(
            default = Label("@com_github_kevinburke_go_bindata//go-bindata"),
            allow_single_file = True,
            executable = True,
            cfg = "exec",
        ),
    },
    toolchains = ["@io_bazel_rules_go//go:toolchain"],
)
