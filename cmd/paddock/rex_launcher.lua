rex.action{
  name = "paddock_new",
  title = "New Paddock Task",
  category = "Paddock",
  keywords = "agent codex claude launch prompt repository",
  run = function(ctx)
    if not ctx.session_id then
      error("Open a Rex session before creating a Paddock task.", 0)
    end
    local command = @LAUNCHER_COMMAND@
    if ctx.client_id then
      command[#command + 1] = "--client"
      command[#command + 1] = ctx.client_id
    end
    local cwd = os.getenv("HOME")
    if ctx.block_id then
      local process = rex.block.call("com.superlogical.terminal", "process", {
        block_id = ctx.block_id,
      })
      if process and process.foreground and process.foreground.cwd then
        cwd = process.foreground.cwd
      end
    end
    local layer, err = rex.session.new_layer{
      bounds = { 0.12, 0.10, 0.76, 0.80 },
      focus = true,
      layout = {
        block = {
          flavor = "com.superlogical.terminal.shell",
          label = "New Paddock Task",
          options = { command = command, cwd = cwd, shell = "none" },
        },
      },
    }
    if not layer then error(err, 0) end
    return layer
  end,
}
rex.bind(@SHORTCUT@, "paddock_new")
