module tailscale.com

go 1.20

require (
	filippo.io/mkcert v1.4.4
	github.com/Microsoft/go-winio v0.6.0
	github.com/akutz/memconn v0.1.0
	github.com/alexbrainman/sspi v0.0.0-20210105120005-909beea2cc74
	github.com/andybalholm/brotli v1.0.4
	github.com/anmitsu/go-shlex v0.0.0-20200514113438-38f4b401e2be
	github.com/aws/aws-sdk-go-v2 v1.17.3
	github.com/aws/aws-sdk-go-v2/config v1.11.0
	github.com/aws/aws-sdk-go-v2/feature/s3/manager v1.7.4
	github.com/aws/aws-sdk-go-v2/service/s3 v1.21.0
	github.com/aws/aws-sdk-go-v2/service/ssm v1.35.0
	github.com/coreos/go-iptables v0.6.0
	github.com/coreos/go-systemd v0.0.0-20191104093116-d3cd4ed1dbcf
	github.com/creack/pty v1.1.17
	github.com/dave/jennifer v1.6.0
	github.com/dblohm7/wingoes v0.0.0-20230131214643-2b26ab7fb5f9
	github.com/dsnet/try v0.0.3
	github.com/evanw/esbuild v0.14.53
	github.com/frankban/quicktest v1.14.3
	github.com/fxamacker/cbor/v2 v2.4.0
	github.com/go-json-experiment/json v0.0.0-20221017203807-c5ed296b8c92
	github.com/go-logr/zapr v1.2.3
	github.com/go-ole/go-ole v1.2.6
	github.com/godbus/dbus/v5 v5.1.0
	github.com/golang/groupcache v0.0.0-20210331224755-41bb18bfe9da
	github.com/golangci/golangci-lint v1.52.2
	github.com/google/go-cmp v0.5.9
	github.com/google/go-containerregistry v0.9.0
	github.com/google/nftables v0.1.1-0.20230115205135-9aa6fdf5a28c
	github.com/google/uuid v1.3.0
	github.com/goreleaser/nfpm v1.10.3
	github.com/hdevalence/ed25519consensus v0.0.0-20220222234857-c00d1f31bab3
	github.com/iancoleman/strcase v0.2.0
	github.com/illarion/gonotify v1.0.1
	github.com/insomniacslk/dhcp v0.0.0-20221215072855-de60144f33f8
	github.com/josharian/native v1.1.1-0.20230202152459-5c7d0dd6ab86
	github.com/jsimonetti/rtnetlink v1.1.2-0.20220408201609-d380b505068b
	github.com/kballard/go-shellquote v0.0.0-20180428030007-95032a82bc51
	github.com/klauspost/compress v1.15.4
	github.com/kortschak/wol v0.0.0-20200729010619-da482cc4850a
	github.com/libdns/libdns v0.2.1
	github.com/mattn/go-colorable v0.1.13
	github.com/mattn/go-isatty v0.0.17
	github.com/mdlayher/genetlink v1.2.0
	github.com/mdlayher/netlink v1.7.1
	github.com/mdlayher/sdnotify v1.0.0
	github.com/miekg/dns v1.1.50
	github.com/mitchellh/go-ps v1.0.0
	github.com/peterbourgon/ff/v3 v3.1.2
	github.com/pkg/errors v0.9.1
	github.com/pkg/sftp v1.13.4
	github.com/prometheus/client_golang v1.14.0
	github.com/prometheus/common v0.41.0
	github.com/skip2/go-qrcode v0.0.0-20200617195104-da1b6568686e
	github.com/tailscale/certstore v0.1.1-0.20220316223106-78d6e1c49d8d
	github.com/tailscale/depaware v0.0.0-20210622194025-720c4b409502
	github.com/tailscale/goexpect v0.0.0-20210902213824-6e8c725cea41
	github.com/tailscale/golang-x-crypto v0.0.0-20221102133106-bc99ab8c2d17
	github.com/tailscale/goupnp v1.0.1-0.20210804011211-c64d0f06ea05
	github.com/tailscale/hujson v0.0.0-20220630195928-54599719472f
	github.com/tailscale/mkctr v0.0.0-20220601142259-c0b937af2e89
	github.com/tailscale/netlink v1.1.1-0.20211101221916-cabfb018fe85
	github.com/tailscale/win v0.0.0-20230202211146-fd21cc0d8ef1
	github.com/tailscale/wireguard-go v0.0.0-20230410165232-af172621b4dd
	github.com/tc-hib/winres v0.1.6
	github.com/tcnksm/go-httpstat v0.2.0
	github.com/toqueteos/webbrowser v1.2.0
	github.com/u-root/u-root v0.9.1-0.20230109201855-948a78c969ad
	github.com/vishvananda/netlink v1.1.1-0.20211118161826-650dca95af54
	go.uber.org/zap v1.24.0
	go4.org/mem v0.0.0-20210711025021-927187094b94
	go4.org/netipx v0.0.0-20220725152314-7e7bdc8411bf
	golang.org/x/crypto v0.6.0
	golang.org/x/exp v0.0.0-20230127140709-cafedaf64729
	golang.org/x/mod v0.9.0
	golang.org/x/net v0.8.0
	golang.org/x/oauth2 v0.5.0
	golang.org/x/sync v0.1.0
	golang.org/x/sys v0.6.0
	golang.org/x/term v0.6.0
	golang.org/x/time v0.0.0-20220609170525-579cf78fd858
	golang.org/x/tools v0.7.0
	golang.zx2c4.com/wintun v0.0.0-20230126152724-0fa3db229ce2
	golang.zx2c4.com/wireguard/windows v0.5.3
	gvisor.dev/gvisor v0.0.0-20230328175328-162ed5ef888d
	honnef.co/go/tools v0.4.3
	inet.af/peercred v0.0.0-20210906144145-0893ea02156a
	inet.af/tcpproxy v0.0.0-20221017015627-91f861402626
	inet.af/wf v0.0.0-20220728202103-50d96caab2f6
	k8s.io/api v0.25.0
	k8s.io/apimachinery v0.25.0
	k8s.io/client-go v0.25.0
	nhooyr.io/websocket v1.8.7
	sigs.k8s.io/controller-runtime v0.13.1
	sigs.k8s.io/yaml v1.3.0
	software.sslmate.com/src/go-pkcs12 v0.2.0
)

require (
	github.com/klauspost/cpuid/v2 v2.1.1 // indirect
	github.com/mholt/acmez v1.0.4 // indirect
	github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common v1.0.597 // indirect
)

require (
	4d63.com/gocheckcompilerdirectives v1.2.1 // indirect
	gopkg.in/Knetic/govaluate.v3 v3.0.0 // indirect
)

require (
	4d63.com/gochecknoglobals v0.2.1 // indirect
	filippo.io/edwards25519 v1.0.0-rc.1 // indirect
	github.com/Abirdcfly/dupword v0.0.11 // indirect
	github.com/Antonboom/errname v0.1.9 // indirect
	github.com/Antonboom/nilnil v0.1.3 // indirect
	github.com/BurntSushi/toml v1.2.1 // indirect
	github.com/Djarvur/go-err113 v0.1.0 // indirect
	github.com/GaijinEntertainment/go-exhaustruct/v2 v2.3.0 // indirect
	github.com/Masterminds/goutils v1.1.1 // indirect
	github.com/Masterminds/semver v1.5.0 // indirect
	github.com/Masterminds/semver/v3 v3.1.1 // indirect
	github.com/Masterminds/sprig v2.22.0+incompatible // indirect
	github.com/OpenPeeDeeP/depguard v1.1.1 // indirect
	github.com/ProtonMail/go-crypto v0.0.0-20221026131551-cf6655e29de4 // indirect
	github.com/PuerkitoBio/purell v1.1.1 // indirect
	github.com/PuerkitoBio/urlesc v0.0.0-20170810143723-de5bf2ad4578 // indirect
	github.com/acomagu/bufpipe v1.0.3 // indirect
	github.com/alexkohler/prealloc v1.0.0 // indirect
	github.com/alingse/asasalint v0.0.11 // indirect
	github.com/ashanbrown/forbidigo v1.5.1 // indirect
	github.com/ashanbrown/makezero v1.1.1 // indirect
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.0.0 // indirect
	github.com/aws/aws-sdk-go-v2/credentials v1.6.4 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.8.2 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.1.27 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.4.21 // indirect
	github.com/aws/aws-sdk-go-v2/internal/ini v1.3.2 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.5.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.5.2 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/s3shared v1.9.2 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.6.2 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.11.1 // indirect
	github.com/aws/smithy-go v1.13.5 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/bkielbasa/cyclop v1.2.0 // indirect
	github.com/blakesmith/ar v0.0.0-20190502131153-809d4375e1fb // indirect
	github.com/blizzy78/varnamelen v0.8.0 // indirect
	github.com/bombsimon/wsl/v3 v3.4.0 // indirect
	github.com/breml/bidichk v0.2.4 // indirect
	github.com/breml/errchkjson v0.3.1 // indirect
	github.com/butuzov/ireturn v0.1.1 // indirect
	github.com/caddyserver/certmagic v0.17.2
	github.com/cavaliercoder/go-cpio v0.0.0-20180626203310-925f9528c45e // indirect
	github.com/cespare/xxhash/v2 v2.2.0 // indirect
	github.com/charithe/durationcheck v0.0.10 // indirect
	github.com/chavacava/garif v0.0.0-20230227094218-b8c73b2037b8 // indirect
	github.com/cloudflare/circl v1.1.0 // indirect
	github.com/containerd/stargz-snapshotter/estargz v0.11.4 // indirect
	github.com/curioswitch/go-reassign v0.2.0 // indirect
	github.com/daixiang0/gci v0.10.1 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/denis-tingaikin/go-header v0.4.3 // indirect
	github.com/docker/cli v20.10.16+incompatible // indirect
	github.com/docker/distribution v2.8.1+incompatible // indirect
	github.com/docker/docker v20.10.16+incompatible // indirect
	github.com/docker/docker-credential-helpers v0.6.4 // indirect
	github.com/emicklei/go-restful/v3 v3.8.0 // indirect
	github.com/emirpasic/gods v1.12.0 // indirect
	github.com/esimonov/ifshort v1.0.4 // indirect
	github.com/ettle/strcase v0.1.1 // indirect
	github.com/evanphx/json-patch v4.12.0+incompatible // indirect
	github.com/evanphx/json-patch/v5 v5.6.0 // indirect
	github.com/fatih/color v1.15.0 // indirect
	github.com/fatih/structtag v1.2.0 // indirect
	github.com/firefart/nonamedreturns v1.0.4 // indirect
	github.com/fsnotify/fsnotify v1.5.4 // indirect
	github.com/fzipp/gocyclo v0.6.0 // indirect
	github.com/gliderlabs/ssh v0.3.3 // indirect
	github.com/go-critic/go-critic v0.7.0 // indirect
	github.com/go-git/gcfg v1.5.0 // indirect
	github.com/go-git/go-billy/v5 v5.3.1 // indirect
	github.com/go-git/go-git/v5 v5.4.2 // indirect
	github.com/go-logr/logr v1.2.3 // indirect
	github.com/go-openapi/jsonpointer v0.19.5 // indirect
	github.com/go-openapi/jsonreference v0.19.5 // indirect
	github.com/go-openapi/swag v0.19.14 // indirect
	github.com/go-toolsmith/astcast v1.1.0 // indirect
	github.com/go-toolsmith/astcopy v1.1.0 // indirect
	github.com/go-toolsmith/astequal v1.1.0 // indirect
	github.com/go-toolsmith/astfmt v1.1.0 // indirect
	github.com/go-toolsmith/astp v1.1.0 // indirect
	github.com/go-toolsmith/strparse v1.1.0 // indirect
	github.com/go-toolsmith/typep v1.1.0 // indirect
	github.com/go-xmlfmt/xmlfmt v1.1.2 // indirect
	github.com/gobwas/glob v0.2.3 // indirect
	github.com/gofrs/flock v0.8.1 // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/golang/protobuf v1.5.2 // indirect
	github.com/golangci/check v0.0.0-20180506172741-cfe4005ccda2 // indirect
	github.com/golangci/dupl v0.0.0-20180902072040-3e9179ac440a // indirect
	github.com/golangci/go-misc v0.0.0-20220329215616-d24fe342adfe // indirect
	github.com/golangci/gofmt v0.0.0-20220901101216-f2edd75033f2 // indirect
	github.com/golangci/lint-1 v0.0.0-20191013205115-297bf364a8e0 // indirect
	github.com/golangci/maligned v0.0.0-20180506175553-b1d89398deca // indirect
	github.com/golangci/misspell v0.4.0 // indirect
	github.com/golangci/revgrep v0.0.0-20220804021717-745bb2f7c2e6 // indirect
	github.com/golangci/unconvert v0.0.0-20180507085042-28b1c447d1f4 // indirect
	github.com/google/btree v1.0.1 // indirect
	github.com/google/gnostic v0.5.7-v3refs // indirect
	github.com/google/gofuzz v1.1.0 // indirect
	github.com/google/goterm v0.0.0-20200907032337-555d40f16ae2 // indirect
	github.com/google/rpmpack v0.0.0-20201206194719-59e495f2b7e1 // indirect
	github.com/gordonklaus/ineffassign v0.0.0-20230107090616-13ace0543b28 // indirect
	github.com/goreleaser/chglog v0.1.2 // indirect
	github.com/goreleaser/fileglob v0.3.1 // indirect
	github.com/gorilla/mux v1.8.0
	github.com/gostaticanalysis/analysisutil v0.7.1 // indirect
	github.com/gostaticanalysis/comment v1.4.2 // indirect
	github.com/gostaticanalysis/forcetypeassert v0.1.0 // indirect
	github.com/gostaticanalysis/nilerr v0.1.1 // indirect
	github.com/hashicorp/errwrap v1.1.0 // indirect
	github.com/hashicorp/go-multierror v1.1.1 // indirect
	github.com/hashicorp/go-version v1.6.0 // indirect
	github.com/hashicorp/hcl v1.0.0 // indirect
	github.com/hexops/gotextdiff v1.0.3 // indirect
	github.com/huandu/xstrings v1.3.2 // indirect
	github.com/imdario/mergo v0.3.12 // indirect
	github.com/inconshreveable/mousetrap v1.0.1 // indirect
	github.com/jbenet/go-context v0.0.0-20150711004518-d14ea06fba99 // indirect
	github.com/jgautheron/goconst v1.5.1 // indirect
	github.com/jingyugao/rowserrcheck v1.1.1 // indirect
	github.com/jirfag/go-printf-func-name v0.0.0-20200119135958-7558a9eaa5af // indirect
	github.com/jmespath/go-jmespath v0.4.0 // indirect
	github.com/josharian/intern v1.0.0 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/julz/importas v0.1.0 // indirect
	github.com/junk1tm/musttag v0.5.0 // indirect
	github.com/kevinburke/ssh_config v1.1.0 // indirect
	github.com/kisielk/errcheck v1.6.3 // indirect
	github.com/kisielk/gotool v1.0.0 // indirect
	github.com/kkHAIKE/contextcheck v1.1.4 // indirect
	github.com/kr/fs v0.1.0 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/kulti/thelper v0.6.3 // indirect
	github.com/kunwardeep/paralleltest v1.0.6 // indirect
	github.com/kyoh86/exportloopref v0.1.11 // indirect
	github.com/ldez/gomoddirectives v0.2.3 // indirect
	github.com/ldez/tagliatelle v0.4.0 // indirect
	github.com/leonklingele/grouper v1.1.1 // indirect
	github.com/libdns/alidns v1.0.2
	github.com/libdns/cloudflare v0.1.0
	github.com/libdns/namesilo v0.1.0
	github.com/libdns/tencentcloud v1.0.0
	github.com/lufeee/execinquery v1.2.1 // indirect
	github.com/magiconair/properties v1.8.6 // indirect
	github.com/mailru/easyjson v0.7.6 // indirect
	github.com/maratori/testableexamples v1.0.0 // indirect
	github.com/maratori/testpackage v1.1.1 // indirect
	github.com/matoous/godox v0.0.0-20230222163458-006bad1f9d26 // indirect
	github.com/mattn/go-runewidth v0.0.13 // indirect
	github.com/matttproud/golang_protobuf_extensions v1.0.4 // indirect
	github.com/mbilski/exhaustivestruct v1.2.0 // indirect
	github.com/mdlayher/socket v0.4.0 // indirect
	github.com/mgechev/revive v1.3.1 // indirect
	github.com/mitchellh/copystructure v1.2.0 // indirect
	github.com/mitchellh/go-homedir v1.1.0 // indirect
	github.com/mitchellh/mapstructure v1.5.0 // indirect
	github.com/mitchellh/reflectwalk v1.0.2 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/moricho/tparallel v0.3.1 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/nakabonne/nestif v0.3.1 // indirect
	github.com/nbutton23/zxcvbn-go v0.0.0-20210217022336-fa2cb2858354 // indirect
	github.com/nfnt/resize v0.0.0-20180221191011-83c6a9932646 // indirect
	github.com/nishanths/exhaustive v0.9.5 // indirect
	github.com/nishanths/predeclared v0.2.2 // indirect
	github.com/nunnatsa/ginkgolinter v0.9.0 // indirect
	github.com/olekukonko/tablewriter v0.0.5 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.0.3-0.20220114050600-8b9d41f48198 // indirect
	github.com/patrickmn/go-cache v2.1.0+incompatible
	github.com/pelletier/go-toml v1.9.5 // indirect
	github.com/pelletier/go-toml/v2 v2.0.5 // indirect
	github.com/pkg/diff v0.0.0-20210226163009-20ebb0f2a09e // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/polyfloyd/go-errorlint v1.4.0 // indirect
	github.com/prometheus/client_model v0.3.0 // indirect
	github.com/prometheus/procfs v0.9.0 // indirect
	github.com/quasilyte/go-ruleguard v0.3.19 // indirect
	github.com/quasilyte/gogrep v0.5.0 // indirect
	github.com/quasilyte/regex/syntax v0.0.0-20210819130434-b3f0c404a727 // indirect
	github.com/quasilyte/stdinfo v0.0.0-20220114132959-f7386bf02567 // indirect
	github.com/rivo/uniseg v0.2.0 // indirect
	github.com/robfig/cron/v3 v3.0.1
	github.com/rogpeppe/go-internal v1.9.0 // indirect
	github.com/rs/zerolog v1.29.0
	github.com/ryancurrah/gomodguard v1.3.0 // indirect
	github.com/ryanrolds/sqlclosecheck v0.4.0 // indirect
	github.com/sanposhiho/wastedassign/v2 v2.0.7 // indirect
	github.com/sashamelentyev/interfacebloat v1.1.0 // indirect
	github.com/sashamelentyev/usestdlibvars v1.23.0 // indirect
	github.com/sassoftware/go-rpmutils v0.1.0 // indirect
	github.com/securego/gosec/v2 v2.15.0 // indirect
	github.com/sergi/go-diff v1.2.0 // indirect
	github.com/shazow/go-diff v0.0.0-20160112020656-b6b7b6733b8c // indirect
	github.com/sirupsen/logrus v1.9.0 // indirect
	github.com/sivchari/containedctx v1.0.2 // indirect
	github.com/sivchari/nosnakecase v1.7.0 // indirect
	github.com/sivchari/tenv v1.7.1 // indirect
	github.com/skratchdot/open-golang v0.0.0-20200116055534-eef842397966
	github.com/sonatard/noctx v0.0.2 // indirect
	github.com/sourcegraph/go-diff v0.7.0 // indirect
	github.com/spf13/afero v1.8.2 // indirect
	github.com/spf13/cast v1.5.0 // indirect
	github.com/spf13/cobra v1.6.1 // indirect
	github.com/spf13/jwalterweatherman v1.1.0 // indirect
	github.com/spf13/pflag v1.0.5 // indirect
	github.com/spf13/viper v1.12.0 // indirect
	github.com/ssgreg/nlreturn/v2 v2.2.1 // indirect
	github.com/stbenjam/no-sprintf-host-port v0.1.1 // indirect
	github.com/stretchr/objx v0.5.0 // indirect
	github.com/stretchr/testify v1.8.2 // indirect
	github.com/subosito/gotenv v1.4.1 // indirect
	github.com/t-yuki/gocover-cobertura v0.0.0-20180217150009-aaee18c8195c // indirect
	github.com/tailscale/walk v0.0.0-20230210175544-f4cdc7d889ba
	github.com/tdakkota/asciicheck v0.2.0 // indirect
	github.com/tetafro/godot v1.4.11 // indirect
	github.com/timakin/bodyclose v0.0.0-20221125081123-e39cf3fc478e // indirect
	github.com/timonwong/loggercheck v0.9.4 // indirect
	github.com/tomarrell/wrapcheck/v2 v2.8.1 // indirect
	github.com/tommy-muehle/go-mnd/v2 v2.5.1 // indirect
	github.com/u-root/uio v0.0.0-20221213070652-c3537552635f // indirect
	github.com/ulikunitz/xz v0.5.10 // indirect
	github.com/ultraware/funlen v0.0.3 // indirect
	github.com/ultraware/whitespace v0.0.5 // indirect
	github.com/uudashr/gocognit v1.0.6 // indirect
	github.com/vbatts/tar-split v0.11.2 // indirect
	github.com/vishvananda/netns v0.0.0-20211101163701-50045581ed74 // indirect
	github.com/x448/float16 v0.8.4 // indirect
	github.com/xanzy/ssh-agent v0.3.1 // indirect
	github.com/yagipy/maintidx v1.0.0 // indirect
	github.com/yeya24/promlinter v0.2.0 // indirect
	gitlab.com/bosi/decorder v0.2.3 // indirect
	go.uber.org/atomic v1.7.0 // indirect
	go.uber.org/multierr v1.6.0 // indirect
	golang.org/x/exp/typeparams v0.0.0-20230224173230-c95f2b4c22f2 // indirect
	golang.org/x/image v0.5.0 // indirect
	golang.org/x/text v0.8.0 // indirect
	gomodules.xyz/jsonpatch/v2 v2.2.0 // indirect
	google.golang.org/appengine v1.6.7 // indirect
	google.golang.org/protobuf v1.30.0 // indirect
	gopkg.in/inf.v0 v0.9.1 // indirect
	gopkg.in/ini.v1 v1.67.0 // indirect
	gopkg.in/warnings.v0 v0.1.2 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	howett.net/plist v1.0.0 // indirect
	k8s.io/apiextensions-apiserver v0.25.0 // indirect
	k8s.io/component-base v0.25.0 // indirect
	k8s.io/klog/v2 v2.70.1 // indirect
	k8s.io/kube-openapi v0.0.0-20220803162953-67bda5d908f1 // indirect
	k8s.io/utils v0.0.0-20220728103510-ee6ede2d64ed // indirect
	mvdan.cc/gofumpt v0.4.0 // indirect
	mvdan.cc/interfacer v0.0.0-20180901003855-c20040233aed // indirect
	mvdan.cc/lint v0.0.0-20170908181259-adc824a0674b // indirect
	mvdan.cc/unparam v0.0.0-20221223090309-7455f1af531d // indirect
	sigs.k8s.io/json v0.0.0-20220713155537-f223a00ba0e2 // indirect
	sigs.k8s.io/structured-merge-diff/v4 v4.2.3 // indirect
)
