#!/usr/bin/env bash
# The starfix test server's entrypoint (deploy/docker/Dockerfile), doing
# what docs/server.md's steps 2 to 6 do on a machine.
#
# On the first start it makes the sshd host key, gives Dolt's starfix
# account a random password, which only the 0600 config file holds, and
# writes the config. On every start it installs the principals given in
# STARFIX_KEYS or /etc/starfix-keys, runs dolt, starfixd and sshd, and
# prints the .starfix.yaml a client needs. When one of the three exits,
# it stops the other two and exits 1.
set -euo pipefail
umask 077

state=/state
config=/etc/starfix/starfixd.yaml
authorized=$state/ssh/authorized_keys
hostkey=$state/hostkeys/ssh_host_ed25519_key
keys_file=/etc/starfix-keys
ready=/run/starfix-ready
port=${STARFIX_PORT:-2222}

name_re='^[a-z][a-z0-9._-]{0,63}$'
uuid_re='^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
type_re='^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp(256|384|521)|sk-ssh-ed25519@openssh\.com|sk-ecdsa-sha2-nistp256@openssh\.com)$'
blob_re='^[A-Za-z0-9+/]+={0,3}$'

pids=()   # children, in the order they started
names=()  # their names, for the log

log() { printf 'starfix-docker: %s\n' "$*" >&2; }

# stop ends the children, last started first, so starfixd closes the store
# before Dolt goes away.
stop() {
  trap - TERM INT
  local i
  for ((i = ${#pids[@]} - 1; i >= 0; i--)); do
    kill -TERM "${pids[i]}" 2>/dev/null || true
    wait "${pids[i]}" 2>/dev/null || true
  done
  pids=()
}

die() {
  log "$1"
  log "fix: $2"
  stop
  exit 1
}

# run_as USER DIR CMD... replaces the shell with CMD, run as USER in DIR
# with a bare environment and no way to gain privileges, as the systemd
# units in docs/server.md do. Call it in a subshell or in the background,
# so the child's pid is the program's own.
run_as() {
  local user=$1 dir=$2
  shift 2
  cd "$dir" || exit 1
  exec env -i PATH=/usr/local/bin:/usr/bin:/bin HOME="$(getent passwd "$user" | cut -d: -f6)" \
    setpriv --reuid="$user" --regid="$user" --init-groups --no-new-privs "$@"
}

# start NAME CMD... runs CMD in the background as a supervised child.
start() {
  local name=$1
  shift
  "$@" &
  pids+=("$!")
  names+=("$name")
}

# wait_for NAME TEST... waits up to 30s for TEST to pass while NAME, the
# newest child, still runs.
wait_for() {
  local name=$1 pid=${pids[-1]} i
  shift
  for ((i = 0; i < 300; i++)); do
    if "$@"; then
      return 0
    fi
    kill -0 "$pid" 2>/dev/null || die "$name exited while starting" "read its log above"
    sleep 0.1
  done
  die "$name did not start within 30s" "read its log above"
}

# shellcheck disable=SC2329 # called through wait_for
port_open() { (: <"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }

random_hex() { od -An -N24 -tx1 /dev/urandom | tr -d ' \n'; }

setting() { sed -n "s/^$1: //p" "$config"; }

# write_config DSN PROJECT ADMINS writes the config file, mode 0600, from
# stdin so the password is never an argument.
write_config() {
  printf 'dsn: %s\nproject: %s\nadmins: [%s]\n' "$1" "$2" "$3" |
    install -o starfix -g starfix -m 0600 /dev/stdin "$config"
}

prepare() {
  rm -f "$ready" /run/sshd.pid
  install -d -o dolt -g dolt -m 0700 "$state/dolt" "$state/dolt/data" "$state/dolt/cfg"
  install -d -o root -g starfix -m 0750 "$state/starfix"
  install -d -o starfix -g starfix -m 0700 "$state/ssh" /run/starfix
  install -d -o root -g root -m 0700 "$state/hostkeys"
  install -d -m 0755 /run/sshd
  if [[ ! -f $hostkey ]]; then
    ssh-keygen -q -t ed25519 -N '' -C starfix-docker -f "$hostkey"
  fi
}

# init_store is step 4: the database, an account with rights on it alone,
# a random password for Dolt's root that nobody keeps, and the config.
init_store() {
  if [[ -d $state/dolt/data/starfix ]]; then
    die "the volume holds a database but no config, from a first start that failed" \
      "remove the volume (docker compose down -v) and start again"
  fi
  local dbpw rootpw project=${STARFIX_PROJECT:-$(cat /proc/sys/kernel/random/uuid)}
  dbpw=$(random_hex)
  rootpw=$(random_hex)
  printf "CREATE DATABASE starfix;\nCREATE USER 'starfix'@'localhost' IDENTIFIED BY '%s';\nGRANT ALL ON starfix.* TO 'starfix'@'localhost';\nALTER USER 'root'@'localhost' IDENTIFIED BY '%s';\n" "$dbpw" "$rootpw" |
    (run_as dolt /var/lib/dolt env DOLT_CLI_PASSWORD= dolt --host 127.0.0.1 --port 3306 --no-tls -u root sql >/dev/null) ||
    die "could not create the database and its account" "read Dolt's log above"
  write_config "starfix:$dbpw@tcp(127.0.0.1:3306)/starfix" "$project" ""
  log "created project $project"
}

# parse_principals reads STARFIX_KEYS and /etc/starfix-keys, one
# "NAME TYPE KEY [COMMENT]" per line, into key_lines and admins
# (STARFIX_ADMINS, or else the first name). It runs before anything is
# written, so a bad line leaves the volume as it was.
key_lines=()
admins=()
parse_principals() {
  local name type blob rest principals=() a
  while read -r name type blob rest; do
    [[ -z $name || $name == \#* ]] && continue
    [[ $name =~ $name_re ]] ||
      die "principal name '$name' is not valid" "use lowercase letters, digits, '.', '_' or '-', starting with a letter"
    case $name in starfixd | import | starfixd-upgrade)
      die "principal name '$name' is reserved" "choose another name" ;;
    esac
    if ! [[ $type =~ $type_re && $blob =~ $blob_re ]] ||
      ! printf '%s %s\n' "$type" "$blob" | ssh-keygen -lf /dev/stdin >/dev/null 2>&1; then
      die "the key for '$name' is not an SSH public key" "give lines like: alice ssh-ed25519 AAAA..."
    fi
    key_lines+=("restrict,command=\"/usr/local/bin/starfixd stdio --principal $name\" $type $blob $name")
    [[ " ${principals[*]} " == *" $name "* ]] || principals+=("$name")
  done < <(
    printf '%s\n' "${STARFIX_KEYS:-}"
    if [[ -r $keys_file ]]; then cat "$keys_file"; fi
  )
  if ((${#key_lines[@]} == 0)); then
    return 0
  fi
  IFS=', ' read -r -a admins <<<"${STARFIX_ADMINS:-${principals[0]}}"
  for a in "${admins[@]}"; do
    [[ $a =~ $name_re ]] || die "admin name '$a' is not valid" "list principal names in STARFIX_ADMINS, comma-separated"
  done
}

# install_principals writes authorized_keys and the config's admins from
# what parse_principals read. Given no keys, it keeps what an earlier
# start installed.
install_principals() {
  if ((${#key_lines[@]} == 0)); then
    return 0
  fi
  printf '%s\n' "${key_lines[@]}" | install -o starfix -g starfix -m 0600 /dev/stdin "$authorized"
  local quoted
  quoted=$(printf '"%s", ' "${admins[@]}")
  write_config "$(setting dsn)" "$(setting project)" "${quoted%, }"
}

report() {
  local fpr project admins principals
  fpr=$(ssh-keygen -lf "$hostkey.pub" | cut -d' ' -f2)
  project=$(setting project)
  admins=$(setting admins | tr -d '[]"')
  principals=$( (sed -n 's/.*--principal \([^"]*\)".*/\1/p' "$authorized" 2>/dev/null || true) | sort -u | paste -sd, - | sed 's/,/, /g')
  if [[ -z $principals ]]; then
    log "no principals, so no one can connect"
    log "fix: set STARFIX_KEYS to \"alice \$(cat ~/.ssh/id_ed25519.pub)\" and start again"
  fi
  cat <<EOF

starfix test server ready (not for production)
  host key    $fpr
  project     $project
  principals  ${principals:-none}
  admins      ${admins}

Save this as .starfix.yaml in a scratch repository:

project: $project
server:
  host: localhost
  port: $port
  host_key: $fpr

EOF
}

main() {
  trap 'stop; exit 0' TERM INT
  parse_principals
  if [[ -n ${STARFIX_PROJECT:-} && ! ${STARFIX_PROJECT} =~ $uuid_re ]]; then
    die "STARFIX_PROJECT is not a lowercase UUID" "unset it for a random one, or give one like 6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f"
  fi
  prepare

  local first=0
  [[ -f $config ]] || first=1
  if ((first)); then
    (run_as dolt /var/lib/dolt sh -c 'dolt config --global --add user.name "starfix server" &&
      dolt config --global --add user.email starfix@localhost &&
      dolt config --global --add metrics.disabled true' >/dev/null)
  fi
  start dolt run_as dolt /var/lib/dolt/data dolt sql-server --config /etc/dolt/config.yaml
  wait_for dolt port_open 3306
  if ((first)); then
    init_store
  elif [[ -n ${STARFIX_PROJECT:-} && ${STARFIX_PROJECT} != "$(setting project)" ]]; then
    die "STARFIX_PROJECT is not the project this volume holds, $(setting project)" \
      "unset STARFIX_PROJECT, or remove the volume (docker compose down -v) for a new project"
  fi
  install_principals

  start starfixd run_as starfix /var/lib/starfix starfixd serve
  wait_for starfixd test -S /run/starfix/starfixd.sock

  /usr/sbin/sshd -t || die "sshd refuses its config" "read the error above"
  start sshd env -i PATH=/usr/sbin:/usr/bin:/bin /usr/sbin/sshd -D -e
  # sshd writes its pid file once it listens; probing the port would log a
  # dropped connection.
  wait_for sshd test -s /run/sshd.pid

  report
  touch "$ready"

  local done_pid status=0 i
  wait -n -p done_pid "${pids[@]}" || status=$?
  for i in "${!pids[@]}"; do
    if [[ ${pids[i]} == "$done_pid" ]]; then
      log "${names[i]} exited with status $status; stopping"
    fi
  done
  stop
  exit 1
}

main "$@"
