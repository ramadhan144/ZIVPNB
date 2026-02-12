#!/bin/bash

# Colors
GREEN="\033[1;32m"
YELLOW="\033[1;33m"
CYAN="\033[1;36m"
RED="\033[1;31m"
RESET="\033[0m"
BOLD="\033[1m"
GRAY="\033[1;30m"

print_task() {
  echo -ne "${GRAY}•${RESET} $1..."
}

print_done() {
  echo -e "\r${GREEN}✓${RESET} $1      "
}

run_silent() {
  local msg="$1"
  local cmd="$2"
  
  print_task "$msg"
  bash -c "$cmd" &>/tmp/udpgw_uninstall.log
  if [ $? -eq 0 ]; then
    print_done "$msg"
  else
    echo -e "\r${RED}✗${RESET} $msg (lihat log: /tmp/udpgw_uninstall.log)"
  fi
}

clear
echo -e "${BOLD}udpgw Uninstaller${RESET}"
echo -e "${GRAY}Ramadan Edition${RESET}"
echo ""

run_silent "Stopping and disabling udpgw service" "systemctl stop udpgw &>/dev/null; systemctl disable udpgw &>/dev/null; killall udpgw &>/dev/null"

run_silent "Removing udpgw files and binaries" "rm -f /usr/local/bin/udpgw; rm -rf /etc/udpgw; rm -f /etc/systemd/system/udpgw.service"

run_silent "Reloading systemd daemon" "systemctl daemon-reload && systemctl daemon-reexec"

echo ""
echo -e "${BOLD}Uninstallation Complete${RESET}"
echo -e "${GRAY}udpgw has been completely removed from your system.${RESET}"

echo ""
