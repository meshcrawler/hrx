package app

const ZshCompletion = `#compdef hrx

_hrx() {
  local state line model='' word
  local -a choices
  local -a latest_args
  local -i i
  if (( CURRENT == 2 )); then
    choices=(
      "--auto:Translate a natural-language launch and confirm"
      "codex:Launch Codex with a validated gateway model"
      "claude:Launch Claude Code through the gateway"
      "opencode:Launch OpenCode through the gateway"
      "models:List authorized Codex models from the local cache"
      "catalog:Full catalog with access and compatibility for every harness"
      "refresh:Refresh shared catalog and key authorization"
      "doctor:Read-only gateway, harness and authentication diagnostics"
      "dashboard:Live local agent dashboard and optional pooled host feeds"
      "sweep:Find leftover dashboard rows, sources and HRX containers (plan only)"
      "hub:Enroll hosts and coordinate agents through PostgreSQL"
      "completion:Print shell completion script"
      "prime:Print the agent-facing guide to hrx"
    )
    _describe 'hrx command' choices
    return
  fi
  case $words[2] in
    dashboard) _arguments '--listen[Loopback IP and port]:address:' '*--source[Collector NAME=URL]:source:' '--hub[Read the authenticated hub fleet]' ;;
    sweep) _arguments '*--host[Proxmox node as SSH host]:host:' '--dashboard[Pooled dashboard URL]:url:' '--idle[Abandoned after this long]:duration:' '--json[Print JSON]' '*--apply[Finding ID to act on]:id:' '--backup-storage[Storage for pre-destroy backups]:storage:' ;;
    hub)
      if (( CURRENT == 3 )); then
        choices=("serve:Run the hub API" "enroll:Enroll this installation" "status:Read fleet status" "collect:Forward local observations" "rotate:Rotate a host credential" "revoke:Revoke a host credential")
        _describe 'hub command' choices
      else
        case $words[3] in
          serve) _arguments '--database-url-file[Private PostgreSQL connection file]:file:_files' '--admin-token-file[Private administrator token]:file:_files' '--viewer-token-file[Private viewer token]:file:_files' '--listen[Listen address]:address:' '--tls-cert[TLS certificate]:file:_files' '--tls-key[TLS private key]:file:_files' ;;
          collect) _arguments '--once[Forward once and exit]' ;;
          enroll) _arguments '--url[Hub origin]:origin:' '--admin-token-file[Private administrator token]:file:_files' '--token-file[Private host credential path]:file:_files' '*--prefix[Authorized identity prefix]:prefix:' ;;
          status) _arguments '--url[Hub origin]:origin:' '--token-file[Private viewer credential]:file:_files' ;;
          rotate|revoke) _arguments '--url[Hub origin]:origin:' '--host-id[Installation ID]:identity:' '--admin-token-file[Private administrator token]:file:_files' '--new-token-file[Replacement credential for rotation]:file:_files' ;;
        esac
      fi
      ;;
    --auto|--auto=*) _arguments '--model[Explicit gateway model]:model:' '--reasoning[Explicit reasoning]:level:' '--agent-identity[Explicit identity]:identity:' '--pitput[Enable Pitput]' '*--pitput-board[Explicit board]:board:' '--gitlab[Enable GitLab service account]' '--yolo[Explicitly bypass approvals]' '--no-yolo[Keep approval prompts]' '--dry-run[Preview after confirmation]' '*:Natural-language request:' ;;
    codex|claude|opencode)
      for (( i=3; i<=${#words}; i++ )); do
        word=$words[i]
        [[ $word == -- ]] && break
        [[ $word == --latest ]] && latest_args=(--latest)
        if [[ $word == --model ]]; then
          model=$words[i+1]
        elif [[ $word == --model=* ]]; then
          model=${word#--model=}
        fi
      done
      _arguments -C \
        '--latest[Hide superseded generations in model completion]' \
        '--model=[Gateway model]:model:->models' \
        '--reasoning=[Supported reasoning effort]:reasoning:->reasoning' \
        '--dry-run[Validate and show routing without launching]' \
        '--plugin=[Enable broker integration]:plugin:(gitlab-service-account-broker)' \
        '--gitlab[Enable GitLab service accounts]' \
        '--no-yolo[Keep permission prompts for this launch]' \
        '--agent-identity=[Agent identity for broker, Pitput, or hub]:identity:' \
        '--pitput[Enable Pitput; automatic delivery for Codex]' \
        '*--pitput-board[Enroll and monitor a Pitput board]:board:' \
        '--pitput-targeted[Notify for recipient mentions; default]' \
        '--pitput-all-activity[Monitor all activity on selected boards]' \
        '*--pitput-follow-thread[Also notify for followed topic]:topic path:' \
        '(--pitput-dm-only)--pitput-no-dm[Do not deliver Pitput direct messages]' \
        '(--pitput-no-dm *--pitput-board)--pitput-dm-only[Deliver only Pitput direct messages]' \
        '--yesod-mail[Register identity and inject Yesod mail guidance]' \
        '--yesod-bbs[Register identity and inject bulletin guidance]' \
        '--yesod-comms[Enable both Yesod mail and bulletin guidance]' \
        '--yesod-agent=[Reuse a registered Yesod identity]:address:' \
        '--yolo[Bypass approvals; OpenCode still respects explicit denies]' \
        '--resume=[Resume a specific harness session]:session:' \
        '--continue[Continue the latest harness session]' \
        '*:Harness argument:_default'
      case $state in
        models)
          choices=("${(@f)$(command hrx _complete model-descriptions "$words[2]" "${latest_args[@]}")}")
          [[ -n $choices[1] ]] || { _message "run hrx refresh --harness $words[2] to populate completion"; return 1; }
          _describe -t models 'authorized model (evidence, not uptime)' choices
          ;;
        reasoning)
          choices=("${(@f)$(command hrx _complete reasoning-descriptions "$words[2]" "$model")}")
          [[ -n $choices[1] ]] || return 1
          _describe -t reasoning 'reasoning effort' choices
          ;;
      esac
      ;;
    models)
      _arguments '--latest[Hide superseded generations]' '--harness[Harness]:harness:(codex claude opencode)' '--refresh[Refresh before listing]' '--json[Print JSON]' '--stale[Explicitly allow an old cache]'
      ;;
    refresh) _arguments '--harness[Harness]:harness:(codex claude opencode)' ;;
    doctor) _arguments '--harness[Harness]:harness:(all codex claude opencode)' '--model[Model or exact route]:model:' '--reasoning[Reasoning level]:effort:' '--json[Structured diagnostics]' '--timeout[Total seconds, 1..60]:seconds:' '--yesod-mail[Check Yesod registry]' '--yesod-bbs[Check Yesod registry]' '--yesod-comms[Check Yesod registry]' ;;
    catalog) _arguments '--compact[One row per model with provider/account shorthand]' '--details[Route/account, authorization, test evidence and limits]' '--stale[Allow historical cache; never proof of current access]' '--latest[Hide superseded generations]' '--harness[Harness]:harness:(all codex claude opencode)' '--refresh[Refresh every selected harness]' '--json[Full structured catalog]' '--search[Filter model or provider]:query:' '--output[Export JSON atomically]:file:_files' ;;
    completion) _values 'shell' zsh ;;
  esac
}

if [[ $funcstack[1] == _hrx ]]; then
  _hrx "$@"
else
  compdef _hrx hrx
fi
`
