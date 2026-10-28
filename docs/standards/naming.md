# Naming convention

## Character set

Repository and binary names use lowercase ASCII letters without separators.
Portable filenames use letters, digits, dots, underscores and hyphens, following
[POSIX portable filenames](https://pubs.opengroup.org/onlinepubs/9799919799/basedefs/V1_chap03.html#tag_03_265).
DNS labels additionally follow [RFC 1123, section 2.1](https://www.rfc-editor.org/rfc/rfc1123.html#section-2.1).

## Grammar and vocabulary

| Identifier | Value |
|---|---|
| Project display name | GoPGRoute |
| Repository | `ioplane/gopgroute` |
| Go module | `github.com/ioplane/gopgroute` |
| Daemon | `gopgroute` |
| Configuration basename | `gopgroute` |

These values are closed. Additional binaries and public package names require
an explicit convention change before implementation.

## Dates and versions

Use RFC 3339 timestamps in metadata and ISO 8601 calendar dates in documents.
Releases follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html);
no version or release tag exists at repository bootstrap.

## Commits and branches

Use [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/).
`master` contains accepted changes; features use `<type>/<short-slug>` and pull
requests. Create `dev` and `release/vMAJOR.MINOR` only when a product line needs
them. Release tags are immutable and never reused.

## Metadata

Environment, site, addresses, ownership and deployment lifecycle belong in
configuration or metadata, outside the project name. Public examples use
[RFC 5737](https://www.rfc-editor.org/rfc/rfc5737.html) documentation addresses.
