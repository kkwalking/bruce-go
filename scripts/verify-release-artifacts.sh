#!/usr/bin/env bash
#
# 校验 dist/ 里的发布产物是否可信。
#
#     scripts/verify-release-artifacts.sh <dist-dir> <version>
#
# <version> 不带 v 前缀（与归档名一致），例如 0.9.0。
#
# 这个脚本是发布的闸门，所以它自己也要被验证：
# `release.yml` 在 tag 上用真实产物跑它，`test.yml` 的 release-dry-run job
# 在每次 push/PR 上用它跑一遍完整发布路径。历史教训见下面 tar 那段注释 ——
# 这个脚本里曾经有一处只在 Linux 复现的 SIGPIPE 失败，本机 macOS 测不出来。
set -euo pipefail

dist="${1:?usage: verify-release-artifacts.sh <dist-dir> <version>}"
version="${2:?usage: verify-release-artifacts.sh <dist-dir> <version>}"

# 平台矩阵与 Makefile 的 PLATFORMS 一致。
platforms="darwin/arm64 linux/amd64"

# macOS 上没有 sha256sum，只有 shasum。
if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$@"; }
else
  sha256() { shasum -a 256 "$@"; }
fi

test -f "${dist}/checksums.txt" || {
  echo "missing ${dist}/checksums.txt" >&2
  exit 1
}

# 顶层断言：不靠"每个文件看起来都对"，而是先确认产物集合就是这个。
# 少一个平台、多一个计划外的归档，都要在这里失败。
expected=""
for platform in ${platforms}; do
  os="${platform%/*}"; arch="${platform#*/}"
  expected="${expected}bruce_${version}_${os}_${arch}.tar.gz "
done
expected="${expected% }"

actual="$(cd "${dist}" && ls -1 ./*.tar.gz 2>/dev/null | sed 's|^\./||' | tr '\n' ' ' | sed 's/ $//')"
test "${actual}" = "${expected}" || {
  echo "unexpected archives: got [${actual}] want [${expected}]" >&2
  exit 1
}

(cd "${dist}" && sha256 -c checksums.txt)

for platform in ${platforms}; do
  os="${platform%/*}"; arch="${platform#*/}"
  archive="${dist}/bruce_${version}_${os}_${arch}.tar.gz"
  work="$(mktemp -d)"
  trap 'rm -rf "${work}"' EXIT

  # 归档解开应当是二进制的平铺，不是带外层目录的嵌套。
  #
  # 先把 listing 收进变量再 grep，不要写成 `tar -tzf ... | grep -q`：
  # grep -q 命中首行即退出，tar 还在写 listing 就吃到 SIGPIPE，
  # 配上 set -o pipefail 会让这一步在 Linux 上稳定返回 141 而失败。
  # GNU tar 如此，BSD tar 未必复现 —— 在 macOS 上跑这个脚本是绿的，
  # 换到 linux/amd64 容器里必红。
  listing="$(tar -tzf "${archive}")"
  grep -qx bruce <<<"${listing}" || {
    echo "${archive}: binary not at archive root" >&2
    exit 1
  }

  tar -xzf "${archive}" -O bruce > "${work}/bruce"
  chmod +x "${work}/bruce"

  # GOOS/GOARCH 内嵌在二进制里，比文件名可信。
  info="$(go version -m "${work}/bruce")"
  grep -q "GOOS=${os}" <<<"${info}" || {
    echo "${archive}: not GOOS=${os}" >&2
    exit 1
  }
  grep -q "GOARCH=${arch}" <<<"${info}" || {
    echo "${archive}: not GOARCH=${arch}" >&2
    exit 1
  }

  # 注入的版本号必须在二进制里。
  #
  # 这是存在性检查，比"跑起来看它报什么"弱：字符串可能来自依赖而不是 -X 注入。
  # 所以只要产物能在当前平台执行，就实跑一次 --version 做交叉验证 ——
  # 本机架构与产物架构一致时（例如 Linux runner 上的 linux/amd64）才算数。
  grep -a -q -F "${version}" "${work}/bruce" || {
    echo "${archive}: version ${version} not embedded" >&2
    exit 1
  }

  if "${work}/bruce" --version >/dev/null 2>&1; then
    reported="$("${work}/bruce" --version)"
    test "${reported}" = "${version}" || {
      echo "${archive}: reports '${reported}', want '${version}'" >&2
      exit 1
    }
    echo "  ${archive}: ok (executed, reports ${reported})"
  else
    echo "  ${archive}: ok (not executable on this host, checks only)"
  fi
done

echo "artifacts ok for ${version}"
