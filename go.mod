module github.com/earthly/earthly

go 1.16

require (
	github.com/alessio/shellescape v1.4.1
	github.com/antlr/antlr4 v0.0.0-20200225173536-225249fdaef5
	github.com/armon/circbuf v0.0.0-20190214190532-5111143e8da2
	github.com/containerd/containerd v1.4.1-0.20201215193253-e922d5553d12
	github.com/creack/pty v1.1.11
	github.com/docker/distribution v2.7.1+incompatible
	github.com/docker/docker v20.10.0-beta1.0.20201110211921-af34b94a78a1+incompatible
	github.com/dustin/go-humanize v1.0.0
	github.com/fatih/color v1.9.0
	github.com/golang/protobuf v1.4.3
	github.com/joho/godotenv v1.3.0
	github.com/mattn/go-isatty v0.0.12
	github.com/moby/buildkit v0.8.2-0.20210129065303-6b9ea0c202cf
	github.com/nu7hatch/gouuid v0.0.0-20131221200532-179d4d0c4d8d
	github.com/opencontainers/go-digest v1.0.0
	github.com/opencontainers/image-spec v1.0.1
	github.com/otiai10/copy v1.1.1
	github.com/pkg/errors v0.9.1
	github.com/seehuhn/password v0.0.0-20131211191456-9ed6612376fa
	github.com/sirupsen/logrus v1.7.0
	github.com/stretchr/testify v1.5.1
	github.com/tonistiigi/fsutil v0.0.0-20201103201449-0834f99b7b85
	github.com/urfave/cli/v2 v2.3.0
	golang.org/x/crypto v0.0.0-20201117144127-c1f2f97bffc9
	golang.org/x/sync v0.0.0-20201207232520-09787c993a3a
	google.golang.org/grpc v1.30.0
	google.golang.org/protobuf v1.24.0
	gopkg.in/yaml.v2 v2.3.0
)

replace (
	// estargz: needs this replace because stargz-snapshotter git repo has two go.mod modules.
	github.com/containerd/stargz-snapshotter/estargz => github.com/containerd/stargz-snapshotter/estargz v0.0.0-20201217071531-2b97b583765b
	github.com/docker/docker => github.com/docker/docker v17.12.0-ce-rc1.0.20200310163718-4634ce647cf2+incompatible
	github.com/hashicorp/go-immutable-radix => github.com/tonistiigi/go-immutable-radix v0.0.0-20170803185627-826af9ccf0fe
	github.com/jaguilar/vt100 => github.com/tonistiigi/vt100 v0.0.0-20190402012908-ad4c4a574305
	github.com/moby/buildkit => github.com/earthly/buildkit v0.7.1-0.20210130000928-c2e9a46c8f51
)
