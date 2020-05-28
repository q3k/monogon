// Copyright 2020 The Monogon Project Authors.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

func depsKubernetes(p *planner) {
	// containerd and its deps
	p.collect(
		"k8s.io/kubernetes", "v1.19.0-alpha.2",
		buildTags("providerless"),
		disabledProtoBuild,
		patches("k8s-kubernetes.patch", "k8s-kubernetes-build.patch"),
	).inject(
		// repo infra, not requested by k8s, but used with bazel
		"k8s.io/repo-infra", "df02ded38f9506e5bbcbf21702034b4fef815f2f",
	).with(patches("k8s-client-go.patch", "k8s-client-go-build.patch")).use(
		"k8s.io/client-go",
	).use(
		"k8s.io/cli-runtime",
		"k8s.io/client-go",
		"k8s.io/cloud-provider",
		"k8s.io/cluster-bootstrap",
		"k8s.io/component-base",
		"k8s.io/csi-translation-lib",
		"k8s.io/kube-controller-manager",
		"k8s.io/kube-proxy",
		"k8s.io/kube-scheduler",
		"k8s.io/kubectl",
		"k8s.io/legacy-cloud-providers",
		"k8s.io/sample-apiserver",
	).with(disabledProtoBuild).use(
		"k8s.io/api",
		"k8s.io/apiextensions-apiserver",
		"k8s.io/apimachinery",
		"k8s.io/apiserver",
		"k8s.io/cri-api",
		"k8s.io/kube-aggregator",
		"k8s.io/kubelet",
		"k8s.io/metrics",
	).use(
		"cloud.google.com/go",
		"github.com/Azure/go-ansiterm",
		"github.com/MakeNowJust/heredoc",
		"github.com/NYTimes/gziphandler",
		"github.com/PuerkitoBio/purell",
		"github.com/PuerkitoBio/urlesc",
		"github.com/armon/circbuf",
		"github.com/asaskevich/govalidator",
		"github.com/bgentry/speakeasy",
		"github.com/blang/semver",
		"github.com/chai2010/gettext-go",
		"github.com/checkpoint-restore/go-criu",
		"github.com/container-storage-interface/spec",
		"github.com/coreos/go-oidc",
		"github.com/coreos/go-semver",
		"github.com/coreos/go-systemd",
		"github.com/coreos/pkg",
		"github.com/cyphar/filepath-securejoin",
		"github.com/daviddengcn/go-colortext",
		"github.com/dgrijalva/jwt-go",
		"github.com/docker/go-connections",
		"github.com/dustin/go-humanize",
		"github.com/euank/go-kmsg-parser",
		"github.com/evanphx/json-patch",
		"github.com/exponent-io/jsonpath",
		"github.com/fatih/camelcase",
		"github.com/fatih/color",
		"github.com/ghodss/yaml",
		"github.com/go-openapi/analysis",
		"github.com/go-openapi/errors",
		"github.com/go-openapi/jsonpointer",
		"github.com/go-openapi/jsonreference",
		"github.com/go-openapi/loads",
		"github.com/go-openapi/runtime",
		"github.com/go-openapi/spec",
		"github.com/go-openapi/strfmt",
		"github.com/go-openapi/swag",
		"github.com/go-openapi/validate",
		"github.com/go-stack/stack",
		"github.com/golang/groupcache",
		"github.com/google/btree",
		"github.com/google/go-cmp",
		"github.com/googleapis/gnostic",
		"github.com/gorilla/websocket",
		"github.com/gregjones/httpcache",
		"github.com/grpc-ecosystem/go-grpc-middleware",
		"github.com/grpc-ecosystem/go-grpc-prometheus",
		"github.com/grpc-ecosystem/grpc-gateway",
		"github.com/jonboulle/clockwork",
		"github.com/karrick/godirwalk",
		"github.com/liggitt/tabwriter",
		"github.com/lithammer/dedent",
		"github.com/mailru/easyjson",
		"github.com/mattn/go-colorable",
		"github.com/mattn/go-isatty",
		"github.com/mattn/go-runewidth",
		"github.com/mindprince/gonvml",
		"github.com/mitchellh/go-wordwrap",
		"github.com/mitchellh/mapstructure",
		"github.com/moby/term",
		"github.com/morikuni/aec",
		"github.com/mrunalp/fileutils",
		"github.com/munnerz/goautoneg",
		"github.com/mxk/go-flowrate",
		"github.com/olekukonko/tablewriter",
		"github.com/peterbourgon/diskv",
		"github.com/pquerna/cachecontrol",
		"github.com/robfig/cron",
		"github.com/soheilhy/cmux",
		"github.com/spf13/afero",
		"github.com/spf13/pflag",
		"github.com/tmc/grpc-websocket-proxy",
		"github.com/vishvananda/netlink",
		"github.com/vishvananda/netns",
		"github.com/xiang90/probing",
		"go.mongodb.org/mongo-driver",
		"go.uber.org/atomic",
		"go.uber.org/multierr",
		"go.uber.org/zap",
		"golang.org/x/xerrors",
		"gonum.org/v1/gonum",
		"gopkg.in/natefinch/lumberjack.v2",
		"gopkg.in/square/go-jose.v2",
		"k8s.io/gengo",
		"k8s.io/heapster",
		"k8s.io/kube-openapi",
		"k8s.io/utils",
		"sigs.k8s.io/apiserver-network-proxy/konnectivity-client",
		"sigs.k8s.io/kustomize",
		"sigs.k8s.io/structured-merge-diff/v3",
		"vbom.ml/util",
	).with(patches("cadvisor.patch", "cadvisor-build.patch")).use(
		"github.com/google/cadvisor",
	).with(disabledProtoBuild).use(
		"go.etcd.io/etcd",
	)
}
