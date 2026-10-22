#!/bin/sh
# Sets the version of the Claude Code plugin and the Gemini CLI extension,
# before tagging a release: scripts/set-version.sh v0.2.0
set -eu
v=${1:?usage: set-version.sh vX.Y.Z}
v=${v#v}
for f in .claude-plugin/plugin.json gemini-extension.json; do
  sed -i.bak "s/\"version\": *\"[^\"]*\"/\"version\": \"$v\"/" "$f" && rm -f "$f.bak"
done
echo "plugin and extension set to $v; commit, then git tag v$v && git push --tags"
