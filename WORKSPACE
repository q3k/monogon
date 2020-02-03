workspace(name = "nexantic")

load("@bazel_tools//tools/build_defs/repo:http.bzl", "http_archive")
load("@bazel_tools//tools/build_defs/repo:git.bzl", "new_git_repository")

# Load skylib

http_archive(
    name = "bazel_skylib",
    sha256 = "97e70364e9249702246c0e9444bccdc4b847bed1eb03c5a3ece4f83dfe6abc44",
    urls = [
        "https://mirror.bazel.build/github.com/bazelbuild/bazel-skylib/releases/download/1.0.2/bazel-skylib-1.0.2.tar.gz",
        "https://github.com/bazelbuild/bazel-skylib/releases/download/1.0.2/bazel-skylib-1.0.2.tar.gz",
    ],
)

load("@bazel_skylib//:workspace.bzl", "bazel_skylib_workspace")

bazel_skylib_workspace()

# Assert minimum Bazel version

load("@bazel_skylib//lib:versions.bzl", "versions")

versions.check(minimum_bazel_version = "2.0.0")

# Go and Gazelle

load("@bazel_tools//tools/build_defs/repo:http.bzl", "http_archive")

http_archive(
    name = "io_bazel_rules_go",
    sha256 = "e88471aea3a3a4f19ec1310a55ba94772d087e9ce46e41ae38ecebe17935de7b",
    urls = [
        "https://storage.googleapis.com/bazel-mirror/github.com/bazelbuild/rules_go/releases/download/v0.20.3/rules_go-v0.20.3.tar.gz",
        "https://github.com/bazelbuild/rules_go/releases/download/v0.20.3/rules_go-v0.20.3.tar.gz",
    ],
)

http_archive(
    name = "bazel_gazelle",
    sha256 = "86c6d481b3f7aedc1d60c1c211c6f76da282ae197c3b3160f54bd3a8f847896f",
    urls = [
        "https://storage.googleapis.com/bazel-mirror/github.com/bazelbuild/bazel-gazelle/releases/download/v0.19.1/bazel-gazelle-v0.19.1.tar.gz",
        "https://github.com/bazelbuild/bazel-gazelle/releases/download/v0.19.1/bazel-gazelle-v0.19.1.tar.gz",
    ],
)

load("@io_bazel_rules_go//go:deps.bzl", "go_register_toolchains", "go_rules_dependencies")

# golang.org/x/sys is overridden by the go_rules protobuf dependency -> declare it first, since
# we need a newer version of it for the netlink package which would fail to compile otherwise.
load("@bazel_gazelle//:deps.bzl", "go_repository")

go_repository(
    name = "org_golang_x_sys",
    importpath = "golang.org/x/sys",
    sum = "h1:ZtoklVMHQy6BFRHkbG6JzK+S6rX82//Yeok1vMlizfQ=",
    version = "v0.0.0-20191018095205-727590c5006e",
)

go_rules_dependencies()

go_register_toolchains(
    go_version = "1.13",
    nogo = "@//:nogo_vet",
)

load("@bazel_gazelle//:deps.bzl", "gazelle_dependencies")

gazelle_dependencies()

# Load Gazelle-generated local dependencies

# gazelle:repository_macro repositories.bzl%go_repositories
load("//:repositories.bzl", "go_repositories")

go_repositories()

# Protobuf

http_archive(
    name = "com_google_protobuf",
    sha256 = "758249b537abba2f21ebc2d02555bf080917f0f2f88f4cbe2903e0e28c4187ed",
    strip_prefix = "protobuf-3.10.0",
    urls = ["https://github.com/protocolbuffers/protobuf/archive/v3.10.0.tar.gz"],
)

load("@com_google_protobuf//:protobuf_deps.bzl", "protobuf_deps")

protobuf_deps()

# External repository filegroup shortcut
all_content = """filegroup(name = "all", srcs = glob(["**"]), visibility = ["//visibility:public"])"""

# Linux kernel

linux_version = "5.4.7"

http_archive(
    name = "linux",
    build_file = "//third_party/linux/external:BUILD.repo",
    patch_args = ["-p1"],
    patches = [
        # Enable built-in cmdline for efistub
        "//third_party/linux/external:0001-x86-Allow-built-in-command-line-to-work-in-early-ker.patch",
    ],
    sha256 = "abc9b21d9146d95853dac35f4c4489a0199aff53ee6eee4b0563d1b37079fcc9",
    strip_prefix = "linux-" + linux_version,
    urls = ["https://cdn.kernel.org/pub/linux/kernel/v5.x/linux-%s.tar.xz" % linux_version],
)

# EDK2

# edk2-stable201908
new_git_repository(
    name = "edk2",
    build_file = "//core/build/edk2:BUILD.repo",
    commit = "37eef91017ad042035090cae46557f9d6e2d5917",
    init_submodules = True,
    remote = "https://github.com/tianocore/edk2",
    shallow_since = "1567048229 +0800",
)

# musl

musl_version = "1.1.24"

http_archive(
    name = "musl",
    build_file_content = all_content,
    sha256 = "1370c9a812b2cf2a7d92802510cca0058cc37e66a7bedd70051f0a34015022a3",
    strip_prefix = "musl-" + musl_version,
    urls = ["https://www.musl-libc.org/releases/musl-%s.tar.gz" % musl_version],
)

# util-linux

util_linux_version = "2.34"

http_archive(
    name = "util_linux",
    build_file_content = all_content,
    sha256 = "1d0c1a38f8c14a2c251681907203cccc78704f5702f2ef4b438bed08344242f7",
    strip_prefix = "util-linux-" + util_linux_version,
    urls = ["https://git.kernel.org/pub/scm/utils/util-linux/util-linux.git/snapshot/util-linux-%s.tar.gz" % util_linux_version],
)

# xfsprogs-dev

xfsprogs_dev_version = "5.2.1"

http_archive(
    name = "xfsprogs_dev",
    build_file_content = all_content,
    patch_args = ["-p1"],
    patches = [
        "//core/build/utils/xfsprogs_dev:0001-Fixes-for-static-compilation.patch",
    ],
    sha256 = "6187f25f1744d1ecbb028b0ea210ad586d0f2dae24e258e4688c67740cc861ef",
    strip_prefix = "xfsprogs-dev-" + xfsprogs_dev_version,
    urls = ["https://git.kernel.org/pub/scm/fs/xfs/xfsprogs-dev.git/snapshot/xfsprogs-dev-%s.tar.gz" % xfsprogs_dev_version],
)

# Kubernetes
k8s_version = "1.16.4"

http_archive(
    name = "kubernetes",
    patch_args = ["-p1"],
    patches = [
        "//core/build/kubernetes:0001-avoid-unexpected-keyword-error-by-using-positional-p.patch",
    ],
    sha256 = "3a49373ba56c73c282deb0cfa2ec7bfcc6bf46acb6992f01319eb703cbf68996",
    urls = ["https://dl.k8s.io/v%s/kubernetes-src.tar.gz" % k8s_version],
)

load("@kubernetes//build:workspace_mirror.bzl", "mirror")

http_archive(
    name = "io_k8s_repo_infra",
    sha256 = "f6d65480241ec0fd7a0d01f432938b97d7395aeb8eefbe859bb877c9b4eafa56",
    strip_prefix = "repo-infra-9f4571ad7242bf3ec4b47365062498c2528f9a5f",
    urls = mirror("https://github.com/kubernetes/repo-infra/archive/9f4571ad7242bf3ec4b47365062498c2528f9a5f.tar.gz"),
)
