#  Copyright 2020 The Monogon Project Authors.
#
#  SPDX-License-Identifier: Apache-2.0
#
#  Licensed under the Apache License, Version 2.0 (the "License");
#  you may not use this file except in compliance with the License.
#  You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
#  Unless required by applicable law or agreed to in writing, software
#  distributed under the License is distributed on an "AS IS" BASIS,
#  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#  See the License for the specific language governing permissions and
#  limitations under the License.

def _build_pure_transition_impl(settings, attr):
    """
    Transition that enables pure, static build of Go binaries.
    """
    return {
        "@io_bazel_rules_go//go/config:pure": True,
        "@io_bazel_rules_go//go/config:static": True,
    }

build_pure_transition = transition(
    implementation = _build_pure_transition_impl,
    inputs = [],
    outputs = [
        "@io_bazel_rules_go//go/config:pure",
        "@io_bazel_rules_go//go/config:static",
    ],
)

def _build_static_transition_impl(settings, attr):
    """
    Transition that enables static builds with CGo and musl for Go binaries.
    """
    return {
        "@io_bazel_rules_go//go/config:static": True,
        "//command_line_option:crosstool_top": "//build/toolchain/musl-host-gcc:musl_host_cc_suite",
    }

build_static_transition = transition(
    implementation = _build_static_transition_impl,
    inputs = [],
    outputs = [
        "@io_bazel_rules_go//go/config:static",
        "//command_line_option:crosstool_top",
    ],
)

def _smalltown_initramfs_impl(ctx):
    """
    Generate an lz4-compressed initramfs based on a label/file list.
    """

    # Generate config file for gen_init_cpio that describes the initramfs to build.
    cpio_list_name = ctx.label.name + ".cpio_list"
    cpio_list = ctx.actions.declare_file(cpio_list_name)

    # Start out with some standard initramfs device files.
    cpio_list_content = [
        "dir /dev 0755 0 0",
        "nod /dev/console 0600 0 0 c 5 1",
        "nod /dev/null 0644 0 0 c 1 3",
        "nod /dev/kmsg 0644 0 0 c 1 11",
        "nod /dev/ptmx 0644 0 0 c 5 2",
    ]

    # Find all directories that need to be created.
    directories_needed = []
    for _, p in ctx.attr.files.items():
        if not p.startswith("/"):
            fail("file {} invalid: must begin with /".format(p))

        # Get all intermediate directories on path to file
        parts = p.split("/")[1:-1]
        directories_needed.append(parts)

    for _, p in ctx.attr.files_cc.items():
        if not p.startswith("/"):
            fail("file {} invalid: must begin with /".format(p))

        # Get all intermediate directories on path to file
        parts = p.split("/")[1:-1]
        directories_needed.append(parts)

    # Extend with extra directories defined by user.
    for p in ctx.attr.extra_dirs:
        if not p.startswith("/"):
            fail("directory {} invalid: must begin with /".format(p))

        parts = p.split("/")[1:]
        directories_needed.append(parts)

    directories = []
    for parts in directories_needed:
        # Turn directory parts [usr, local, bin] into successive subpaths [/usr, /usr/local, /usr/local/bin].
        last = ""
        for part in parts:
            last += "/" + part

            # TODO(q3k): this is slow - this should be a set instead, but starlark doesn't implement them.
            # For the amount of files we're dealing with this doesn't matter, but all stars are pointing towards this
            # becoming accidentally quadratic at some point in the future.
            if last not in directories:
                directories.append(last)

    # Append instructions to create directories.
    # Serendipitously, the directories should already be in the right order due to us not using a set to create the
    # list. They might not be in an elegant order (ie, if files [/foo/one/one, /bar, /foo/two/two] are request, the
    # order will be [/foo, /foo/one, /bar, /foo/two]), but that's fine.
    for d in directories:
        cpio_list_content.append("dir {} 0755 0 0".format(d))

    # Append instructions to add files.
    inputs = []
    for label, p in ctx.attr.files.items():
        # Figure out if this is an executable.
        is_executable = True

        di = label[DefaultInfo]
        if di.files_to_run.executable == None:
            # Generated non-executable files will have DefaultInfo.files_to_run.executable == None
            is_executable = False
        elif di.files_to_run.executable.is_source:
            # Source files will have executable.is_source == True
            is_executable = False

        # Ensure only single output is declared.
        # If you hit this error, figure out a better logic to find what file you need, maybe looking at providers other
        # than DefaultInfo.
        files = di.files.to_list()
        if len(files) > 1:
            fail("file {} has more than one output: {}", p, files)
        src = files[0]
        inputs.append(src)

        mode = "0755" if is_executable else "0444"

        cpio_list_content.append("file {} {} {} 0 0".format(p, src.path, mode))

    for label, p in ctx.attr.files_cc.items():
        # Figure out if this is an executable.
        is_executable = True

        di = label[DefaultInfo]
        if di.files_to_run.executable == None:
            # Generated non-executable files will have DefaultInfo.files_to_run.executable == None
            is_executable = False
        elif di.files_to_run.executable.is_source:
            # Source files will have executable.is_source == True
            is_executable = False

        # Ensure only single output is declared.
        # If you hit this error, figure out a better logic to find what file you need, maybe looking at providers other
        # than DefaultInfo.
        files = di.files.to_list()
        if len(files) > 1:
            fail("file {} has more than one output: {}", p, files)
        src = files[0]
        inputs.append(src)

        mode = "0755" if is_executable else "0444"

        cpio_list_content.append("file {} {} {} 0 0".format(p, src.path, mode))

    # Write cpio_list.
    ctx.actions.write(cpio_list, "\n".join(cpio_list_content))

    gen_init_cpio = ctx.executable._gen_init_cpio
    savestdout = ctx.executable._savestdout
    lz4 = ctx.executable._lz4

    # Generate 'raw' (uncompressed) initramfs
    initramfs_raw_name = ctx.label.name
    initramfs_raw = ctx.actions.declare_file(initramfs_raw_name)
    ctx.actions.run(
        outputs = [initramfs_raw],
        inputs = [cpio_list] + inputs,
        tools = [savestdout, gen_init_cpio],
        executable = savestdout,
        arguments = [initramfs_raw.path, gen_init_cpio.path, cpio_list.path],
    )

    # Compress raw initramfs using lz4c.
    initramfs_name = ctx.label.name + ".lz4"
    initramfs = ctx.actions.declare_file(initramfs_name)
    ctx.actions.run(
        outputs = [initramfs],
        inputs = [initramfs_raw],
        tools = [savestdout, lz4],
        executable = lz4.path,
        arguments = ["-l", initramfs_raw.path, initramfs.path],
    )

    return [DefaultInfo(files = depset([initramfs]))]

smalltown_initramfs = rule(
    implementation = _smalltown_initramfs_impl,
    doc = """
        Build a Smalltown initramfs. The initramfs will contain a basic /dev directory and all the files specified by the
        `files` attribute. Executable files will have their permissions set to 0755, non-executable files will have
        their permissions set to 0444. All parent directories will be created with 0755 permissions.
    """,
    attrs = {
        "files": attr.label_keyed_string_dict(
            mandatory = True,
            allow_files = True,
            doc = """
                Dictionary of Labels to String, placing a given Label's output file in the initramfs at the location
                specified by the String value. The specified labels must only have a single output.
            """,
            # Attach pure transition to ensure all binaries added to the initramfs are pure/static binaries.
            cfg = build_pure_transition,
        ),
        "files_cc": attr.label_keyed_string_dict(
            allow_files = True,
            doc = """
                 Special case of 'files' for compilation targets that need to be built with the musl toolchain like
                 go_binary targets which need cgo or cc_binary targets.
            """,
            # Attach static transition to all files_cc inputs to ensure they are built with musl and static.
            cfg = build_static_transition,
        ),
        "extra_dirs": attr.string_list(
            default = [],
            doc = """
                Extra directories to create. These will be created in addition to all the directories required to
                contain the files specified in the `files` attribute.
            """,
        ),

        # Tools, implicit dependencies.
        "_gen_init_cpio": attr.label(
            default = Label("@linux//:gen_init_cpio"),
            executable = True,
            cfg = "host",
        ),
        "_lz4": attr.label(
            default = Label("@com_github_lz4_lz4//programs:lz4"),
            executable = True,
            cfg = "host",
        ),
        "_savestdout": attr.label(
            default = Label("//build/savestdout"),
            executable = True,
            cfg = "host",
        ),

        # Allow for transitions to be attached to this rule.
        "_whitelist_function_transition": attr.label(
            default = "@bazel_tools//tools/whitelists/function_transition_whitelist",
        ),
    },
)
