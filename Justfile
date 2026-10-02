# -----------------------------------------------------------------------------
# Project settings
# -----------------------------------------------------------------------------

set working-directory := './'

edge_proto_dir := 'backend/proto'
modules := './backend ./cli ./types'

# Match the standard-library group in .golangci.yml until gci supports Go 1.27.
go_stdlib_section := 'prefix(arena,archive/,bufio,bytes,cmp,compress/,container/,context,crypto,database/,debug/,embed,encoding,errors,expvar,flag,fmt,go/,hash,html,image,index/,io,iter,log,maps,math,mime,net,os,path,plugin,reflect,regexp,runtime,slices,sort,strconv,strings,structs,sync,syscall,testing,text/,time,unicode,unique,unsafe,uuid,weak)'

_default:
    @just --list

# -----------------------------------------------------------------------------
# Development
# -----------------------------------------------------------------------------

# Run frontend dev server on port 3000
[group('development')]
_dev_frontend:
    vp -C frontend run dev

# Run backend with hot reload on port 3552
[group('development')]
_dev_backend:
    cd backend && air

[group('development')]
_dev_agent:
    #!/usr/bin/env bash
    set -euo pipefail

    if [ -z "${AGENT_TOKEN:-}" ]; then
        echo "AGENT_TOKEN is required. Run: AGENT_TOKEN=<edge-environment-token> just dev agent"
        exit 1
    fi

    port="${PORT:-3553}"
    app_url="${APP_URL:-http://localhost:${port}}"
    manager_api_url="${MANAGER_API_URL:-https://localhost:3552}"
    edge_mtls_assets_dir="${EDGE_MTLS_ASSETS_DIR:-./.tmp/edge-test-agent/edge-mtls-agent}"
    edge_mtls_ca_file="${EDGE_MTLS_CA_FILE:-./backend/local-manager.crt}"
    database_url="${DATABASE_URL:-file:./.tmp/edge-test-agent/arcane.db?_pragma=journal_mode(WAL)&_pragma=busy_timeout(2500)&_txlock=immediate}"
    projects_directory="${PROJECTS_DIRECTORY:-./.tmp/edge-test-agent/projects}"
    git_work_dir="${GIT_WORK_DIR:-./.tmp/edge-test-agent/git}"
    jwt_secret="${JWT_SECRET:-local-edge-test-jwt-secret-please-change}"
    encryption_key="${ENCRYPTION_KEY:-local-edge-test-encryption-key-32}"

    mkdir -p "${projects_directory}" "${git_work_dir}" "${edge_mtls_assets_dir}"

    PORT="${port}" \
    APP_URL="${app_url}" \
    EDGE_AGENT=true \
    EDGE_TRANSPORT=poll \
    EDGE_MTLS_MODE=required \
    EDGE_MTLS_ASSETS_DIR="${edge_mtls_assets_dir}" \
    EDGE_MTLS_CA_FILE="${edge_mtls_ca_file}" \
    AGENT_TOKEN="${AGENT_TOKEN}" \
    MANAGER_API_URL="${manager_api_url}" \
    DATABASE_URL="${database_url}" \
    PROJECTS_DIRECTORY="${projects_directory}" \
    GIT_WORK_DIR="${git_work_dir}" \
    JWT_SECRET="${jwt_secret}" \
    ENCRYPTION_KEY="${encryption_key}" \
    go run ./backend/cmd

[group('development')]
_dev_all:
    #!/usr/bin/env bash
    set -euo pipefail

    shutdown() {
        status=$?
        trap - EXIT
        trap '' INT TERM
        kill -TERM "$backend_pid" "$frontend_pid" 2>/dev/null || true
        wait "$backend_pid" "$frontend_pid" 2>/dev/null || true
        exit "$status"
    }

    (cd backend && exec air) &
    backend_pid=$!
    vp -C frontend run dev &
    frontend_pid=$!
    trap shutdown EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM
    wait "$frontend_pid"

# Rebuild Docker dev environment
[group('development')]
_dev_docker:
    ./scripts/development/dev.sh rebuild

# View Docker dev environment logs
[group('development')]
_dev_logs:
    ./scripts/development/dev.sh logs

# Run development servers. Valid targets: "frontend", "backend", "agent", "all", "docker", "logs".
[group('development')]
dev target="docker":
    @just "_dev_{{ target }}"

# -----------------------------------------------------------------------------
# Build
# -----------------------------------------------------------------------------

# Build the frontend
[group('build')]
_build_frontend:
    vp -C frontend run build

# Build the backend
[group('build')]
_build_backend:
    cd backend && go build ./...

# Build both frontend and backend
[group('build')]
_build_all:
    @just _build_frontend
    @just _build_backend

# Build manager container image
[group('build')]
_build_image_manager tag="ghcr.io/getarcaneapp/arcane:development" flag='':
    docker buildx build {{ if flag == "--push" { "--push" } else { "" } }} --platform linux/arm64,linux/amd64,linux/arm/v7 -f 'docker/Dockerfile' --build-arg ENABLED_FEATURES="{{ env('ENABLED_FEATURES', env('BUILD_FEATURES', '')) }}" -t "{{ tag }}" .

# Build agent container image
[group('build')]
_build_image_agent tag="ghcr.io/getarcaneapp/agent:development" flag='':
    docker buildx build {{ if flag == "--push" { "--push" } else { "" } }} --platform linux/arm64,linux/amd64,linux/arm/v7 -f 'docker/Dockerfile-agent' --build-arg ENABLED_FEATURES="{{ env('ENABLED_FEATURES', env('BUILD_FEATURES', '')) }}" -t "{{ tag }}" .

# Build application code: single {frontend|backend|all}; containers: image {manager|agent} [tag] [--push]
[group('build')]
build buildtype type="" tag="" flag="":
    @if [ "{{ buildtype }}" = "single" ]; then just _build_{{ type }}; \
    elif [ "{{ buildtype }}" = "image" ]; then just _build_image_{{ type }} "{{ if tag != "" { tag } else if type == "manager" { "arcane:latest" } else { "arcane-agent:latest" } }}" "{{ flag }}"; \
    else echo "Unknown build target: {{ buildtype }}. Try: just build single|image" >&2; exit 1; \
    fi

# -----------------------------------------------------------------------------
# Test
# -----------------------------------------------------------------------------

# Run Playwright E2E tests
[group('checks')]
_test_e2e:
    vp -C tests run test

# Run backend Go tests
[group('checks')]
_test_backend:
    #!/usr/bin/env bash
    set -euo pipefail

    cd backend
    if [ -n "${GO_JUNIT_REPORT_FILE:-}" ]; then
        mkdir -p "$(dirname "$GO_JUNIT_REPORT_FILE")"
        go test -json -tags=exclude_frontend,buildables -ldflags "-X github.com/getarcaneapp/arcane/backend/v2/buildables.EnabledFeatures=autologin" ./... -race -coverprofile=coverage.txt -covermode=atomic -v 2>&1 | go run github.com/jstemmer/go-junit-report/v2@v2.1.0 -parser gojson -set-exit-code -out "$GO_JUNIT_REPORT_FILE"
    else
        go test -tags=exclude_frontend,buildables -ldflags "-X github.com/getarcaneapp/arcane/backend/v2/buildables.EnabledFeatures=autologin" ./... -race -coverprofile=coverage.txt -covermode=atomic -v
    fi

# Run CLI tests
[group('checks')]
_test_cli:
    #!/usr/bin/env bash
    set -euo pipefail

    cd cli
    if [ -n "${GO_JUNIT_REPORT_FILE:-}" ]; then
        mkdir -p "$(dirname "$GO_JUNIT_REPORT_FILE")"
        go test -json ./... -race -coverprofile=coverage.txt -covermode=atomic -v 2>&1 | go run github.com/jstemmer/go-junit-report/v2@v2.1.0 -parser gojson -set-exit-code -out "$GO_JUNIT_REPORT_FILE"
    else
        go test ./... -race -coverprofile=coverage.txt -covermode=atomic -v
    fi

# Run shared types Go tests
[group('checks')]
_test_types:
    #!/usr/bin/env bash
    set -euo pipefail

    cd types
    if [ -n "${GO_JUNIT_REPORT_FILE:-}" ]; then
        mkdir -p "$(dirname "$GO_JUNIT_REPORT_FILE")"
        go test -json ./... -race -coverprofile=coverage.txt -covermode=atomic -v 2>&1 | go run github.com/jstemmer/go-junit-report/v2@v2.1.0 -parser gojson -set-exit-code -out "$GO_JUNIT_REPORT_FILE"
    else
        go test ./... -race -coverprofile=coverage.txt -covermode=atomic -v
    fi

[group('checks')]
_test_all:
    @just _test_e2e
    @just _test_backend
    @just _test_cli
    @just _test_types

# Run tests. Valid targets: "e2e", "backend", "cli", "types", "all".
[group('checks')]
test target="all":
    @just "_test_{{ target }}"

# -----------------------------------------------------------------------------
# Quality: format, lint, and fixes
# -----------------------------------------------------------------------------

# Format frontend/test/email TypeScript with vp fmt (Vite+) and Go modules with gci and gofumpt
[group('checks')]
_format_frontend:
    vp fmt frontend

[group('checks')]
_format_js:
    vp fmt tests
    vp fmt email-templates

[group('checks')]
_format_go:
    #!/usr/bin/env bash
    set -euo pipefail
    for module in {{ modules }}; do
        echo "Formatting Go module: ${module} (gci + gofumpt)"
        (cd "$module" && gci write --skip-generated --skip-vendor --custom-order -s "{{ go_stdlib_section }}" -s default -s localmodule .)
        gofumpt -w -extra "$module"
    done

[group('checks')]
_format_just:
    just --fmt --unstable

[group('checks')]
_format_check_frontend:
    vp fmt --check frontend

[group('checks')]
_format_check_js:
    vp fmt --check tests
    vp fmt --check email-templates

[group('checks')]
_format_check_go:
    #!/usr/bin/env bash
    set -euo pipefail

    unformatted=$(
        for module in {{ modules }}; do
            (cd "$module" && gci list --skip-generated --skip-vendor --custom-order -s "{{ go_stdlib_section }}" -s default -s localmodule .) || exit 1
        done
        gofumpt -l -extra {{ modules }}
    )
    if [ -n "$unformatted" ]; then
        echo "Unformatted Go files:"
        echo "$unformatted"
        exit 1
    fi

[group('checks')]
_format_all:
    #!/usr/bin/env bash
    # Run every formatter even if one fails, so e.g. a vp/pnpm hiccup can't skip gofumpt
    failed=0
    for target in frontend js go just; do
        just "_format_${target}" || failed=1
    done
    exit "${failed}"

[group('checks')]
_format_check_all:
    @just _format_check_frontend
    @just _format_check_js
    @just _format_check_go

# Format targets. Valid: "frontend", "js", "go", "just", "all". Use --check to verify formatting.
[group('checks')]
format target="all" check="":
    @if [ "{{ check }}" = "--check" ]; then just "_format_check_{{ target }}"; else just "_format_{{ target }}"; fi

# Type check/Lint frontend
[group('checks')]
_lint_frontend:
    vp -C frontend run check

# Type check Playwright tests
[group('checks')]
_lint_tests:
    vp -C tests run check

# Type check email templates
[group('checks')]
_lint_email:
    vp -C email-templates run check

# Type check all JavaScript/TypeScript workspaces
[group('checks')]
_lint_js:
    @just _lint_frontend
    @just _lint_tests
    @just _lint_email

# Build golangci-lint with the custom linters enabled by the shared config
[group('checks')]
_build_golangci_lint:
    golangci-lint custom

# Lint Go backend
[group('checks')]
_lint_backend:
    cd backend && golangci-lint run -c ../.golangci.yml ./...

# Lint Go CLI
[group('checks')]
_lint_cli:
    cd cli && golangci-lint run -c ../.golangci.yml ./...

# Lint Types
[group('checks')]
_lint_types:
    cd types && golangci-lint run -c ../.golangci.yml ./...

# Lint edge tunnel protobuf definitions.
[group('checks')]
_lint_proto:
    cd {{ edge_proto_dir }} && go run github.com/bufbuild/buf/cmd/buf@latest lint

# Lint all Go code
[group('checks')]
_lint_go: _lint_backend _lint_cli _lint_types

[group('checks')]
_lint_all:
    @just _lint_js
    @just _lint_go
    @just _lint_proto

# Lint targets. Valid: "backend", "frontend", "tests", "email", "js", "cli", "types", "go", "proto", "all".
[group('checks')]
lint target="all":
    @just "_lint_{{ target }}"

# Fix Go backend
[group('checks')]
_fix_backend:
    cd backend && go fix ./...

# Fix Go CLI
[group('checks')]
_fix_cli:
    cd cli && go fix ./...

# Fix Types
[group('checks')]
_fix_types:
    cd types && go fix ./...

# Fix all Go code
[group('checks')]
_fix_go: _fix_backend _fix_cli _fix_types

[group('checks')]
_fix_all:
    @just _fix_go

# Fix targets. Valid: "backend", "cli", "types", "go", "all".
[group('checks')]
fix target="all":
    @just "_fix_{{ target }}"

# -----------------------------------------------------------------------------
# Security
# -----------------------------------------------------------------------------

# Run Snyk against all projects including dev dependencies
[group('checks')]
_snyk_scan:
    snyk test --all-projects --dev --policy-path=.snyk

# Snyk targets. Valid: "scan".
[group('checks')]
snyk target="scan":
    @just "_snyk_{{ target }}"

# -----------------------------------------------------------------------------
# Dependencies
# -----------------------------------------------------------------------------

# Install Node.js workspace dependencies
[group('dependencies')]
_deps_install_frontend:
    vp install

# Install tests dependencies
[group('dependencies')]
_deps_install_tests:
    vp -C tests install
    vp -C tests exec playwright install --with-deps chromium

# Install backend Go dependencies
[group('dependencies')]
_deps_install_backend:
    cd backend && go mod download && go mod tidy && go mod verify
    go work sync

# Install CLI Go dependencies
[group('dependencies')]
_deps_install_cli:
    cd cli && go mod download && go mod tidy && go mod verify
    go work sync

# Install types Go dependencies
[group('dependencies')]
_deps_install_types:
    cd types && go mod download && go mod tidy && go mod verify
    go work sync

# Install all Go dependencies
[group('dependencies')]
_deps_install_go: _deps_install_backend _deps_install_cli _deps_install_types

# Install all Node.js dependencies
[group('dependencies')]
_deps_install_node: _deps_install_frontend _deps_install_tests

# Install all dependencies
[group('dependencies')]
_deps_install_all: _deps_install_node _deps_install_go

# Update frontend dependencies
[group('dependencies')]
_deps_update_frontend:
    vp update

# Update backend Go dependencies
[group('dependencies')]
_deps_update_backend:
    cd backend && go get -u ./... && go mod tidy

# Update direct Go dependencies in all modules, then sync the workspace (requires jq).
[group('dependencies')]
_deps_update_go *args:
    #!/usr/bin/env bash
    set -euo pipefail
    version=upgrade
    set -- {{ args }}
    case "$#:$*" in
        0:) ;;
        1:--patch) version=patch ;;
        *) echo 'Usage: just deps go update [--patch]' >&2; exit 1 ;;
    esac
    command -v jq >/dev/null || { echo 'jq is required.' >&2; exit 1; }
    for module in {{ modules }}; do
        (
            cd "$module"
            export GOWORK=off
            echo "Updating direct dependencies in $module..."
            direct=$(go list -mod=mod -m -json all | jq -r '
                select(.Main != true and .Indirect != true)
                | select(.Replace == null or .Replace.Version != null)
                | .Path')
            dependencies=()
            while IFS= read -r dependency; do
                if [ -n "$dependency" ]; then
                    dependencies+=("$dependency@$version")
                fi
            done <<<"$direct"
            if [ "${#dependencies[@]}" -gt 0 ]; then
                go get "${dependencies[@]}"
            fi
            go mod tidy
        )
        go work sync
    done

# Update Node.js and direct Go dependencies
[group('dependencies')]
_deps_update_all: _deps_update_frontend _deps_update_go

# Dedupe all pnpm workspace dependencies
[group('dependencies')]
_deps_dedupe_node:
    vp dedupe

[group('dependencies')]
_deps_dedupe_all: _deps_dedupe_node

# Manage dependencies: {install|update|dedupe} [target], or go update [--patch]
[group('dependencies')]
deps action="update" target="all" *args:
    #!/usr/bin/env bash
    set -euo pipefail
    if [ "{{ action }}" = "go" ]; then
        if [ "{{ target }}" != "update" ]; then
            echo 'Usage: just deps go update [--patch]' >&2
            exit 1
        fi
        just _deps_update_go {{ args }}
    else
        just "_deps_{{ action }}_{{ target }}" {{ args }}
    fi

# -----------------------------------------------------------------------------
# Code generation
# -----------------------------------------------------------------------------

# Generate edge tunnel protobuf/gRPC code.
[group('generation')]
_generate_proto:
    cd {{ edge_proto_dir }} && go run github.com/bufbuild/buf/cmd/buf@latest lint
    cd {{ edge_proto_dir }} && go run github.com/bufbuild/buf/cmd/buf@latest generate

# Generate targets. Valid: "proto".
[group('generation')]
generate target:
    @just "_generate_{{ target }}"

# -----------------------------------------------------------------------------
# Documentation and localization
# -----------------------------------------------------------------------------

# Generate the docs config schema JSON.
[group('generation')]
_docs_config output="" source_root=".":
    #!/usr/bin/env bash
    set -euo pipefail

    cmd=(go run -tags exclude_frontend ./backend/cmd config-schema --source-root "{{ source_root }}")
    if [ -n "{{ output }}" ]; then
        cmd+=(--output "{{ output }}")
    fi

    "${cmd[@]}"

# Docs targets. Example: just docs config
[group('generation')]
docs target *args:
    @just "_docs_{{ target }}" {{ args }}

# Add an i18n locale
[group('generation')]
_i18n_add locale native_name settings="frontend/project.inlang/settings.json" picker="frontend/src/lib/components/locale-picker.svelte" messages_dir="frontend/messages" base_locale="en":
    #!/usr/bin/env bash
    set -euo pipefail

    if [ -z "{{ locale }}" ] || [ -z "{{ native_name }}" ]; then
        echo "Usage: just i18n add <locale> <native_name> [settings] [picker] [messages_dir] [base_locale]"
        exit 1
    fi

    settings_path="{{ settings }}"
    picker_path="{{ picker }}"
    messages_dir="{{ messages_dir }}"
    base_locale="{{ base_locale }}"
    base_file="${messages_dir}/${base_locale}.json"
    target_file="${messages_dir}/{{ locale }}.json"

    if [ ! -f "$settings_path" ]; then
        echo "Settings file not found: $settings_path"
        exit 1
    fi

    if [ ! -f "$picker_path" ]; then
        echo "Locale picker file not found: $picker_path"
        exit 1
    fi

    if [ ! -f "$base_file" ]; then
        echo "Base messages file not found: $base_file"
        exit 1
    fi

    if ! command -v jq >/dev/null 2>&1; then
        echo "jq is required to update $settings_path"
        exit 1
    fi

    jq_tab="--tab"
    if ! jq --tab -n '{}' >/dev/null 2>&1; then
        jq_tab=""
    fi

    settings_tmp="$(mktemp)"
    jq $jq_tab --arg locale "{{ locale }}" \
        '.locales |= ( . + [$locale] | unique | sort_by(ascii_downcase) )' \
        "$settings_path" > "$settings_tmp"
    mv "$settings_tmp" "$settings_path"

    if ! command -v rg >/dev/null 2>&1; then
        echo "rg (ripgrep) is required to update $picker_path"
        exit 1
    fi

    start_line="$(rg -n -F "const locales: Record<string, string> = {" "$picker_path" | head -n1 | cut -d: -f1)"
    if [ -z "$start_line" ]; then
        echo "Unable to find locales map in $picker_path"
        exit 1
    fi

    end_line="$(awk -v s="$start_line" 'NR>=s && $0 ~ /^[[:space:]]*};/ { print NR; exit }' "$picker_path")"
    if [ -z "$end_line" ]; then
        echo "Unable to find end of locales map in $picker_path"
        exit 1
    fi

    const_indent="$(sed -n "${start_line}p" "$picker_path" | sed -E 's/^([[:space:]]*).*/\1/')"
    entry_indent="$(sed -n "$((start_line+1)),$((end_line-1))p" "$picker_path" | awk 'NF { match($0, /^[[:space:]]*/); print substr($0, RSTART, RLENGTH); exit }')"
    if [ -z "$entry_indent" ]; then
        entry_indent="${const_indent}\t"
    fi

    entries_tmp="$(mktemp)"
    while IFS= read -r line; do
        if [[ $line =~ ^[[:space:]]*\'?([^\'\":]+)\'?[[:space:]]*:[[:space:]]*\'(.*)\'[[:space:]]*,[[:space:]]*$ ]]; then
            key="${BASH_REMATCH[1]}"
            value="${BASH_REMATCH[2]}"
            if [ "$key" != "{{ locale }}" ]; then
                printf '%s\t%s\n' "$key" "$value" >> "$entries_tmp"
            fi
        fi
    done < <(sed -n "$((start_line+1)),$((end_line-1))p" "$picker_path")

    printf '%s\t%s\n' "{{ locale }}" "{{ native_name }}" >> "$entries_tmp"

    new_block="${const_indent}const locales: Record<string, string> = {"
    new_block+=$'\n'
    while IFS=$'\t' read -r key value; do
        [ -z "$key" ] && continue
        if [[ $key =~ ^[A-Za-z_$][A-Za-z0-9_$]*$ ]]; then
            out_key="$key"
        else
            esc_key="${key//\\/\\\\}"
            esc_key="${esc_key//\'/\\\'}"
            out_key="'${esc_key}'"
        fi
        esc_value="${value//\\/\\\\}"
        esc_value="${esc_value//\'/\\\'}"
        new_block+="${entry_indent}${out_key}: '${esc_value}',"
        new_block+=$'\n'
    done < <(LC_ALL=C sort -f -t $'\t' -k1,1 "$entries_tmp")
    new_block+="${const_indent}};"
    rm -f "$entries_tmp"

    block_tmp="$(mktemp)"
    printf '%s\n' "$new_block" > "$block_tmp"

    picker_tmp="$(mktemp)"
    sed -n "1,$((start_line-1))p" "$picker_path" > "$picker_tmp"
    cat "$block_tmp" >> "$picker_tmp"
    tail -n "+$((end_line+1))" "$picker_path" >> "$picker_tmp"
    mv "$picker_tmp" "$picker_path"
    rm -f "$block_tmp"

    formatting_path="frontend/src/lib/utils/formatting.ts"
    if [ ! -f "$formatting_path" ]; then
        echo "Warning: $formatting_path not found; add the date-fns loader for '{{ locale }}' manually."
    elif rg -q "date-fns/locale/{{ locale }}'" "$formatting_path"; then
        echo "date-fns loader for '{{ locale }}' already present in $formatting_path"
    else
        f_start="$(rg -n -F "const dateFnsLocaleLoaders" "$formatting_path" | head -n1 | cut -d: -f1)"
        if [ -z "$f_start" ]; then
            echo "Warning: unable to find dateFnsLocaleLoaders in $formatting_path; add '{{ locale }}' manually."
        else
            f_end="$(awk -v s="$f_start" 'NR>s && $0 ~ /^[[:space:]]*};/ { print NR; exit }' "$formatting_path")"
            if [ -z "$f_end" ]; then
                echo "Warning: unable to find end of dateFnsLocaleLoaders in $formatting_path; add '{{ locale }}' manually."
            else
                f_const_indent="$(sed -n "${f_start}p" "$formatting_path" | sed -E 's/^([[:space:]]*).*/\1/')"
                f_entry_indent="$(sed -n "$((f_start+1)),$((f_end-1))p" "$formatting_path" | awk 'NF { match($0, /^[[:space:]]*/); print substr($0, RSTART, RLENGTH); exit }')"
                if [ -z "$f_entry_indent" ]; then
                    f_entry_indent="${f_const_indent}	"
                fi

                f_entries="$(mktemp)"
                while IFS= read -r line; do
                    if [[ $line =~ ^[[:space:]]*\'?([^\'\":]+)\'?:.*date-fns/locale/([A-Za-z-]+) ]]; then
                        key="${BASH_REMATCH[1]}"
                        mod="${BASH_REMATCH[2]}"
                        if [ "$key" != "{{ locale }}" ]; then
                            printf '%s\t%s\n' "$key" "$mod" >> "$f_entries"
                        fi
                    fi
                done < <(sed -n "$((f_start+1)),$((f_end-1))p" "$formatting_path")
                printf '%s\t%s\n' "{{ locale }}" "{{ locale }}" >> "$f_entries"

                f_block="$(sed -n "${f_start}p" "$formatting_path")"
                f_block+=$'\n'
                while IFS=$'\t' read -r key mod; do
                    [ -z "$key" ] && continue
                    if [[ $key =~ ^[A-Za-z_$][A-Za-z0-9_$]*$ ]]; then
                        out_key="$key"
                    else
                        out_key="'${key}'"
                    fi
                    f_block+="${f_entry_indent}${out_key}: () => resolveDateFnsLocale(() => import('date-fns/locale/${mod}')),"
                    f_block+=$'\n'
                done < <(LC_ALL=C sort -f -t $'\t' -k1,1 "$f_entries")
                f_block+="${f_const_indent}};"
                rm -f "$f_entries"

                f_tmp="$(mktemp)"
                sed -n "1,$((f_start-1))p" "$formatting_path" > "$f_tmp"
                printf '%s\n' "$f_block" >> "$f_tmp"
                tail -n "+$((f_end+1))" "$formatting_path" >> "$f_tmp"
                mv "$f_tmp" "$formatting_path"
                echo "Added date-fns loader for '{{ locale }}' to $formatting_path"

                if [ ! -e "frontend/node_modules/date-fns/locale/{{ locale }}.js" ] && [ ! -d "frontend/node_modules/date-fns/locale/{{ locale }}" ]; then
                    echo "Warning: date-fns may not ship a '{{ locale }}' locale (check the module name, e.g. en uses en-US)."
                fi
            fi
        fi
    fi

    if [ -f "$target_file" ]; then
        echo "Messages file already exists, not overwriting: $target_file"
    else
        cp "$base_file" "$target_file"
        echo "Created messages file: $target_file"
    fi

# Localization targets: add <locale> <native_name>. Example: just i18n add es "Español"
[group('generation')]
i18n target *args:
    @just "_i18n_{{ target }}" {{ args }}

# -----------------------------------------------------------------------------
# Benchmarks
# -----------------------------------------------------------------------------

# Benchmark edge tunnel transports with allocation counts
[group('performance')]
_bench_edge count="3" benchtime="2s":
    cd backend && go test -run '^$' -bench '^BenchmarkEdgeTunnelProxyRequest$' -benchmem -count={{ count }} -benchtime={{ benchtime }} ./pkg/libarcane/edge

# Benchmark edge tunnel transports and write a memory profile
[group('performance')]
_bench_memory profile="edge_tunnel.mem.out" benchtime="5s":
    cd backend && go test -run '^$' -bench '^BenchmarkEdgeTunnelProxyRequest$' -benchmem -benchtime={{ benchtime }} -memprofile={{ profile }} ./pkg/libarcane/edge

# Benchmark edge transports: edge [count] [benchtime], or memory [profile] [benchtime]
[group('performance')]
bench target="edge" *args:
    @just "_bench_{{ target }}" {{ args }}

# -----------------------------------------------------------------------------
# Release
# -----------------------------------------------------------------------------

# Compute the next semver next-image version (e.g. 2.4.0-next.1) from unreleased commits.
# Bump rules: breaking -> major, feat -> minor, other included commits -> patch.
# The -next.N counter continues from tags already published to GHCR; set
# GHCR_TAGS (newline separated) to bypass the registry query for testing.
#
# Compute the next image version; use github-output to export workflow outputs
[group('release')]
_release_version mode="":
    #!/usr/bin/env bash
    set -euo pipefail

    CLIFF_CMD=""
    if command -v git-cliff &>/dev/null; then
        CLIFF_CMD="git-cliff"
    elif git cliff --version &>/dev/null; then
        CLIFF_CMD="git cliff"
    else
        echo "Error: git cliff is not installed. Please install it from https://git-cliff.org/docs/installation." >&2
        exit 1
    fi

    if ! command -v jq &>/dev/null; then
        echo "Error: jq is required." >&2
        exit 1
    fi

    PREVIOUS_TAG=$(git tag -l 'v[0-9]*' --sort=-v:refname | head -n1)
    BASE_VERSION="${PREVIOUS_TAG#v}"
    if [ -z "$PREVIOUS_TAG" ]; then
        BASE_VERSION="0.0.0"
    fi

    CONTEXT=$($CLIFF_CMD --unreleased --context --offline --config cliff.toml)

    BREAKING=$(jq '[.[].commits[]? | select(.breaking == true)] | length' <<<"$CONTEXT")
    FEATURES=$(jq '[.[].commits[]? | select((.group // "") | test("New features"))] | length' <<<"$CONTEXT")

    IFS='.' read -r MAJOR MINOR PATCH <<<"$BASE_VERSION"
    if [ "$BREAKING" -gt 0 ]; then
        NEXT_BASE="$((MAJOR + 1)).0.0"
    elif [ "$FEATURES" -gt 0 ]; then
        NEXT_BASE="${MAJOR}.$((MINOR + 1)).0"
    else
        NEXT_BASE="${MAJOR}.${MINOR}.$((PATCH + 1))"
    fi

    GHCR_IMAGE="${GHCR_IMAGE:-getarcaneapp/manager}"
    if [ -n "${GHCR_TAGS+x}" ]; then
        TAGS="$GHCR_TAGS"
    else
        TOKEN=$(curl -fsSL "https://ghcr.io/token?scope=repository:${GHCR_IMAGE}:pull" | jq -r '.token')
        TAGS=""
        URL="https://ghcr.io/v2/${GHCR_IMAGE}/tags/list?n=1000"
        HEADERS_FILE=$(mktemp)
        while [ -n "$URL" ]; do
            PAGE=$(curl -fsSL -D "$HEADERS_FILE" -H "Authorization: Bearer ${TOKEN}" "$URL")
            TAGS+=$'\n'"$(jq -r '.tags[]?' <<<"$PAGE")"
            NEXT_LINK=$(awk -F'[<>]' 'tolower($0) ~ /^link:/ { print $2 }' "$HEADERS_FILE" | tr -d '\r')
            if [ -n "$NEXT_LINK" ]; then
                URL="https://ghcr.io${NEXT_LINK}"
            else
                URL=""
            fi
        done
        rm -f "$HEADERS_FILE"
    fi

    ESCAPED_BASE="${NEXT_BASE//./\\.}"
    MAX_N=$(grep -E "^v${ESCAPED_BASE}-next\.[0-9]+$" <<<"$TAGS" | sed -E 's/.*-next\.//' | sort -n | tail -n1 || true)
    COUNTER=$(( ${MAX_N:-0} + 1 ))

    VERSION="${NEXT_BASE}-next.${COUNTER}"
    IMAGE_TAG="v${VERSION}"

    echo "previous_tag=${PREVIOUS_TAG}"
    echo "version=${VERSION}"
    echo "image_tag=${IMAGE_TAG}"

    if [ "{{ mode }}" = "github-output" ]; then
        printf '%s\n' \
            "previous_tag=${PREVIOUS_TAG}" \
            "version=${VERSION}" \
            "image_tag=${IMAGE_TAG}" >> "${GITHUB_OUTPUT:?GITHUB_OUTPUT is not set}"
    fi

# Compute the next image version: version [github-output]
[group('release')]
release target="version" *args:
    @just "_release_{{ target }}" {{ args }}

# -----------------------------------------------------------------------------
# Repository maintenance
# -----------------------------------------------------------------------------

# Clean build artifacts
[group('maintenance')]
_repo_clean:
    rm -rf frontend/.svelte-kit frontend/build backend/.bin
    find . -type d -name node_modules -prune -exec rm -rf {} \;

# Repo targets. Valid: "clean".
[group('maintenance')]
repo target="clean":
    @just "_repo_{{ target }}"

# List open feature requests by votes
[group('maintenance')]
_utils_feats:
    #!/usr/bin/env bash
    set -euo pipefail

    if ! command -v gh >/dev/null 2>&1; then
        echo "GitHub CLI is required. Install it from https://cli.github.com/." >&2
        exit 1
    fi

    if ! gh auth status --hostname github.com >/dev/null 2>&1; then
        echo "GitHub CLI is not authenticated. Run: gh auth login" >&2
        exit 1
    fi

    discussions=$(
        gh api graphql \
            --paginate \
            --slurp \
            -f owner=getarcaneapp \
            -f name=arcane \
            -F endCursor=null \
            -f query='query($owner: String!, $name: String!, $endCursor: String) { repository(owner: $owner, name: $name) { discussions(first: 100, after: $endCursor, states: [OPEN]) { nodes { number title url category { slug } isAnswered upvoteCount reactionGroups { content users { totalCount } } } pageInfo { hasNextPage endCursor } } } }' \
        | jq -r 'map(.data.repository.discussions.nodes[] | select(.category.slug == "feature-requests") | . + {upvotes: (.upvoteCount + ([.reactionGroups[]? | select(.content == "THUMBS_UP") | .users.totalCount] | add // 0))}) | sort_by(.upvotes, .number) | reverse | .[] | [.upvotes, .number, (if .isAnswered then "answered" else "open" end), .title, .url] | @tsv'
    )

    if [ -z "$discussions" ]; then
        echo "No open feature request discussions found."
        exit 0
    fi

    echo "Open feature request discussions by votes (upvotes + 👍):"
    echo ""

    while IFS=$'\t' read -r votes number status title url; do
        printf "%3d votes - #%-4s [%s] %s\n" "$votes" "$number" "$status" "$title"
        printf "         %s\n\n" "$url"
    done <<< "$discussions"

# List fix commits since the latest release
[group('maintenance')]
_utils_fixes:
    #!/usr/bin/env bash
    set -euo pipefail

    TEST=false
    VERBOSE=false
    for arg in "$@"; do
        case "$arg" in
        --test)
            TEST=true
            ;;
        --verbose)
            VERBOSE=true
            ;;
        *)
            ;;
        esac
    done

    if [ "$VERBOSE" == true ]; then
        set -x
    fi

    # Colors for output
    GREEN='\033[0;32m'
    YELLOW='\033[1;33m'
    BLUE='\033[0;34m'
    NC='\033[0m' # No Color

    # Get the latest release tag
    LATEST_TAG=$(git describe --tags --abbrev=0 2>/dev/null || echo "")

    if [ -z "$LATEST_TAG" ]; then
        echo -e "${YELLOW}No previous release tag found. Showing all fix commits:${NC}"
        RANGE="HEAD"
    else
        echo -e "${GREEN}Latest release: ${LATEST_TAG}${NC}"
        RANGE="${LATEST_TAG}..HEAD"
    fi

    echo ""
    echo -e "${BLUE}=== Fix commits on main branch since ${LATEST_TAG:-beginning} ===${NC}"
    echo ""

    # List all fix commits
    FIX_COMMITS=$(git log "$RANGE" \
        --oneline \
        --no-merges \
        --grep="^fix:" \
        --grep="^hotfix:" \
        --regexp-ignore-case \
        --pretty=format:"%C(yellow)%h%Creset %C(green)%ai%Creset %s %C(dim)(%an)%Creset" || echo "")

    if [ -z "$FIX_COMMITS" ]; then
        echo "No fix commits found."
    else
        echo "$FIX_COMMITS"
    fi

    if [ "$TEST" == true ]; then
        echo "Test mode: no changes were made."
    fi

# Utils targets. Valid: "feats", "fixes".
[group('maintenance')]
utils target *args:
    @just "_utils_{{ target }}" {{ args }}
