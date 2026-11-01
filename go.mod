module gogen

go 1.27.0

require (
	charm.land/bubbles/v2 v2.2.1
	charm.land/bubbletea/v2 v2.1.1
	charm.land/lipgloss/v2 v2.0.6
	codeberg.org/readeck/go-readability/v2 v2.1.3
	github.com/JohannesKaufmann/html-to-markdown/v2 v2.5.2
	github.com/atotto/clipboard v0.1.4
	github.com/charmbracelet/x/ansi v0.11.9
	github.com/creack/pty v1.1.24
	github.com/ericchiang/css v1.4.0
	github.com/fsnotify/fsnotify v1.9.0
	github.com/gorilla/websocket v1.5.3
	github.com/gortexhq/tree-sitter-dockerfile v0.1.0
	github.com/makiuchi-d/gozxing v0.1.1
	github.com/openai/openai-go/v3 v3.89.0
	github.com/skip2/go-qrcode v0.0.0-20200617195104-da1b6568686e
	github.com/tiktoken-go/tokenizer v0.10.0
	github.com/tree-sitter-grammars/tree-sitter-hcl v1.2.0
	github.com/tree-sitter-grammars/tree-sitter-kotlin v1.1.0
	github.com/tree-sitter-grammars/tree-sitter-lua v0.5.0
	github.com/tree-sitter-grammars/tree-sitter-make v1.1.1
	github.com/tree-sitter-grammars/tree-sitter-toml v0.7.0
	github.com/tree-sitter-grammars/tree-sitter-yaml v0.7.2
	github.com/tree-sitter-grammars/tree-sitter-zig v1.1.2
	github.com/tree-sitter/go-tree-sitter v0.25.0
	github.com/tree-sitter/tree-sitter-bash v0.25.1
	github.com/tree-sitter/tree-sitter-c v0.24.2
	github.com/tree-sitter/tree-sitter-c-sharp v0.23.5
	github.com/tree-sitter/tree-sitter-cpp v0.23.4
	github.com/tree-sitter/tree-sitter-css v0.25.0
	github.com/tree-sitter/tree-sitter-go v0.25.0
	github.com/tree-sitter/tree-sitter-html v0.23.2
	github.com/tree-sitter/tree-sitter-java v0.23.5
	github.com/tree-sitter/tree-sitter-javascript v0.25.0
	github.com/tree-sitter/tree-sitter-json v0.24.8
	github.com/tree-sitter/tree-sitter-php v0.25.1
	github.com/tree-sitter/tree-sitter-python v0.25.0
	github.com/tree-sitter/tree-sitter-ruby v0.23.1
	github.com/tree-sitter/tree-sitter-rust v0.24.2
	github.com/tree-sitter/tree-sitter-scala v0.26.3
	github.com/tree-sitter/tree-sitter-typescript v0.23.2
	github.com/wippyai/tree-sitter-sql v0.0.4
	golang.org/x/net v0.62.0
	golang.org/x/sys v0.49.0
	golang.org/x/time v0.16.0
	gopkg.in/yaml.v3 v3.0.1
	mvdan.cc/sh/v3 v3.14.1
)

require (
	github.com/BurntSushi/toml v1.6.0 // indirect
	github.com/JohannesKaufmann/dom v0.3.1 // indirect
	github.com/andybalholm/cascadia v1.3.5 // indirect
	github.com/charmbracelet/colorprofile v0.4.3 // indirect
	github.com/charmbracelet/ultraviolet v0.0.0-20261008173134-6b8d4baf91b4 // indirect
	github.com/charmbracelet/x/term v0.2.2 // indirect
	github.com/charmbracelet/x/termios v0.1.1 // indirect
	github.com/charmbracelet/x/windows v0.2.2 // indirect
	github.com/clipperhouse/displaywidth v0.11.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/coder/websocket v1.8.15 // indirect
	github.com/dlclark/regexp2/v2 v2.8.0 // indirect
	github.com/fzipp/gocyclo v0.6.0 // indirect
	github.com/go-shiori/dom v0.0.0-20230515143342-73569d674e1c // indirect
	github.com/gogs/chardet v0.0.0-20211120154057-b7413eaefb8f // indirect
	github.com/itlightning/dateparse v0.2.1 // indirect
	github.com/lucasb-eyer/go-colorful v1.4.1 // indirect
	github.com/mattn/go-pointer v0.0.1 // indirect
	github.com/mattn/go-runewidth v0.0.30 // indirect
	github.com/muesli/cancelreader v0.2.2 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/tidwall/gjson v1.19.0 // indirect
	github.com/tidwall/match v1.2.0 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	github.com/xo/terminfo v1.0.0 // indirect
	golang.org/x/exp/typeparams v0.0.0-20260908205506-85c1c2202aba // indirect
	golang.org/x/mod v0.42.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/telemetry v0.0.0-20261010193011-f71dc92da45e // indirect
	golang.org/x/term v0.47.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	golang.org/x/tools v0.52.0 // indirect
	golang.org/x/vuln v1.9.0 // indirect
	golang.org/x/xerrors v0.0.0-20200804184101-5ec99f83aff1 // indirect
	honnef.co/go/tools v0.8.2 // indirect
)

tool (
	github.com/fzipp/gocyclo/cmd/gocyclo
	golang.org/x/vuln/cmd/govulncheck
	honnef.co/go/tools/cmd/staticcheck
)
