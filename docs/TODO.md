# TODO

Parse selected SSH connection options directly in the CLI for SSH-capable commands.
Before implementation, review the SSH options and features and explicitly decide which
to support and which to reject. Pay particular attention to options that change PTY
allocation, session lifetime, or routing, and to conflicts with sshore options.

Lower priority: allow a Windows hosted shell to act as the intermediate hop in a nested shell route.

Directory synchronization (`pull` / `push`) remains a proposal. Revisit deletion
opt-in, conflict handling and optional watching before implementing it.

Add an optional foreground editor shortcut for launch review and other multiline
editors. Use the invoking shell's `EDITOR`, return to preview after editing, and
keep `e` for inline readline editing.
