# Changelog

## Unreleased

- Build the sshore command from `./cmd/sshore`. The installer URLs and binary name stay the same.

## [v0.11.0](https://github.com/jay-waves/sshore/releases/tag/v0.11.0)

- Highlight the selected file across the current column in `lf`.
- Keep the `lp` shortcut bar at the bottom and show `j/k` for movement.

## Migration from JSON history

- Shell, mount, and deployment history use bbolt. Older JSON history is not imported.
