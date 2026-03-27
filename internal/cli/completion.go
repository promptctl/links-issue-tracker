package cli

const bashCompletionScript = `# bash completion for lit
_lit_completions() {
  local current prev words cword
  _init_completion || return

  local commands="init ready new ls show update fix-priority start done close open archive delete unarchive restore comment label parent children dep export sync doctor fsck backup recover bulk workspace hooks migrate quickstart completion help"
  local comment_subcommands="add"
  local label_subcommands="add rm"
  local parent_subcommands="set clear"
  local dep_subcommands="add rm ls"
  local sync_subcommands="status remote fetch pull push"
  local sync_remote_subcommands="ls"
  local backup_subcommands="create list restore"
  local bulk_subcommands="label close archive import"
  local hooks_subcommands="install"

  case "${prev}" in
    lit)
      COMPREPLY=( $(compgen -W "${commands}" -- "${current}") )
      return
      ;;
    comment)
      COMPREPLY=( $(compgen -W "${comment_subcommands}" -- "${current}") )
      return
      ;;
    label)
      COMPREPLY=( $(compgen -W "${label_subcommands}" -- "${current}") )
      return
      ;;
    parent)
      COMPREPLY=( $(compgen -W "${parent_subcommands}" -- "${current}") )
      return
      ;;
    dep)
      COMPREPLY=( $(compgen -W "${dep_subcommands}" -- "${current}") )
      return
      ;;
    sync)
      COMPREPLY=( $(compgen -W "${sync_subcommands}" -- "${current}") )
      return
      ;;
    remote)
      if [[ "${words[1]}" == "sync" ]]; then
        COMPREPLY=( $(compgen -W "${sync_remote_subcommands}" -- "${current}") )
        return
      fi
      ;;
    backup)
      COMPREPLY=( $(compgen -W "${backup_subcommands}" -- "${current}") )
      return
      ;;
    bulk)
      COMPREPLY=( $(compgen -W "${bulk_subcommands}" -- "${current}") )
      return
      ;;
    hooks)
      COMPREPLY=( $(compgen -W "${hooks_subcommands}" -- "${current}") )
      return
      ;;
    completion)
      COMPREPLY=( $(compgen -W "bash zsh fish" -- "${current}") )
      return
      ;;
  esac

  COMPREPLY=( $(compgen -W "${commands}" -- "${current}") )
}

complete -F _lit_completions lit
`

const zshCompletionScript = `#compdef lit

_lit() {
  local -a commands
  commands=(
    'new:create issue'
    'init:initialize links and auto-migrate beads'
    'ls:list issues'
    'show:show issue'
    'update:update issue fields'
    'fix-priority:fix priority inversions'
    'ready:list ready issues'
    'start:claim issue (open->in_progress)'
    'done:mark issue closed (in_progress->closed)'
    'close:close issue'
    'open:reopen issue'
    'archive:archive issue'
    'delete:delete issue'
    'unarchive:unarchive issue'
    'restore:restore deleted issue'
    'comment:add comment'
    'label:manage labels'
    'parent:manage parent relationship'
    'children:list child issues'
    'dep:manage dependencies'
    'export:write JSON export to stdout'
    'sync:sync via dolt remotes'
    'doctor:database health check'
    'fsck:database integrity check and repair'
    'backup:snapshot management'
    'recover:restore from backup or sync file'
    'bulk:bulk issue operations'
    'workspace:show workspace info'
    'hooks:install links git hooks'
    'migrate:adopt lit from beads'
    'quickstart:agent usage guide'
    'completion:emit shell completion'
    'help:show help'
  )

  local context state state_descr line
  _arguments '1:command:->command' '2:subcommand:->subcommand'

  case $state in
    command)
      _describe 'command' commands
      ;;
    subcommand)
      case $line[1] in
        comment)
          _values 'comment commands' add
          ;;
        label)
          _values 'label commands' add rm
          ;;
        parent)
          _values 'parent commands' set clear
          ;;
        dep)
          _values 'dep commands' add rm ls
          ;;
        sync)
          _values 'sync commands' status remote fetch pull push
          ;;
        remote)
          if [[ "$line[1]" = "sync" ]]; then
            _values 'sync remote commands' ls
          fi
          ;;
        backup)
          _values 'backup commands' create list restore
          ;;
        bulk)
          _values 'bulk commands' label close archive import
          ;;
        hooks)
          _values 'hooks commands' install
          ;;
        completion)
          _values 'shell' bash zsh fish
          ;;
      esac
      ;;
  esac
}

_lit "$@"
`

const fishCompletionScript = `complete -c lit -f
complete -c lit -n '__fish_use_subcommand' -a 'init ready new ls show update fix-priority start done close open archive delete unarchive restore comment label parent children dep export sync doctor fsck backup recover bulk workspace hooks migrate quickstart completion help'
complete -c lit -n '__fish_seen_subcommand_from comment' -a 'add'
complete -c lit -n '__fish_seen_subcommand_from label' -a 'add rm'
complete -c lit -n '__fish_seen_subcommand_from parent' -a 'set clear'
complete -c lit -n '__fish_seen_subcommand_from dep' -a 'add rm ls'
complete -c lit -n '__fish_seen_subcommand_from sync' -a 'status remote fetch pull push'
complete -c lit -n '__fish_seen_subcommand_from remote' -a 'ls'
complete -c lit -n '__fish_seen_subcommand_from backup' -a 'create list restore'
complete -c lit -n '__fish_seen_subcommand_from bulk' -a 'label close archive import'
complete -c lit -n '__fish_seen_subcommand_from hooks' -a 'install'
complete -c lit -n '__fish_seen_subcommand_from completion' -a 'bash zsh fish'
`
