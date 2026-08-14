#!/bin/sh
# Regenerate the Helm chart's CRD templates from the generated CRDs.
#
# The chart ships its own copy of every CRD, wrapped in two Helm conditionals
# (crd.enable, crd.keep) and given the chart's labels. kubebuilder's helm plugin
# writes those copies, but nothing in `make manifests` or `make generate` does —
# so the copies drift the moment an API field is added, and a chart install then
# rejects the very samples in this repository with "unknown field". Which is a
# confusing failure: the CRD is present and the field is documented.
#
# It had drifted by three whole features (workspace, expose, toolPolicyRefs)
# before anyone noticed, because `helm lint` cannot see staleness — a stale CRD
# is perfectly valid YAML. Hence this script, and the --check mode that CI runs
# to make the drift fail a build instead of surfacing at install time.
#
#   hack/chart-crds.sh          # write the templates
#   hack/chart-crds.sh --check  # fail if they are out of date
set -eu

SRC=config/crd/bases
DST=dist/chart/templates/crd
CHECK=0
[ "${1:-}" = "--check" ] && CHECK=1

if [ ! -d "$SRC" ] || [ ! -d "$DST" ]; then
  echo "run this from the repository root ($SRC or $DST is missing)" >&2
  exit 1
fi

# The transformation is three insertions into the generated file, and nothing
# else — deliberately: any rewriting beyond this would make the chart's schema
# differ from the one the Operator was generated against, which is the drift this
# script exists to prevent.
render() {
  awk '
    NR == 1 { print "{{- if .Values.crd.enable }}" }
    # The chart labels go under metadata:, and the resource-policy annotation
    # under the annotations: key controller-gen always emits.
    /^metadata:$/ && !seen_meta {
      print
      print "  labels:"
      print "    {{- include \"chart.labels\" . | nindent 4 }}"
      seen_meta = 1
      next
    }
    /^  annotations:$/ && seen_meta && !seen_anno {
      print
      print "    {{- if .Values.crd.keep }}"
      print "    \"helm.sh/resource-policy\": keep"
      print "    {{- end }}"
      seen_anno = 1
      next
    }
    { print }
    END { print "{{- end -}}" }
  ' "$1"
}

status=0
for src in "$SRC"/*.yaml; do
  name=$(basename "$src")
  dst="$DST/$name"
  if [ "$CHECK" = "1" ]; then
    if [ ! -f "$dst" ]; then
      echo "chart CRD missing: $dst" >&2
      status=1
    elif ! render "$src" | diff -q - "$dst" >/dev/null 2>&1; then
      echo "chart CRD out of date: $dst" >&2
      status=1
    fi
  else
    render "$src" > "$dst"
  fi
done

# A CRD removed from config/crd/bases has to leave the chart too, or the chart
# keeps installing a type the Operator no longer serves.
for dst in "$DST"/*.yaml; do
  name=$(basename "$dst")
  [ -f "$SRC/$name" ] && continue
  if [ "$CHECK" = "1" ]; then
    echo "chart CRD has no source and should be deleted: $dst" >&2
    status=1
  else
    rm -f "$dst"
  fi
done

if [ "$status" != "0" ]; then
  echo "" >&2
  echo "run 'make chart-crds' and commit the result" >&2
  exit 1
fi
