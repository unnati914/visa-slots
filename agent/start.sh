#!/bin/sh
# Keeps the Mac awake while the agent runs (caffeinate), then starts it.
# Output goes to the screen and to agent.log.
cd "$(dirname "$0")"
caffeinate -is node agent.js 2>&1 | tee -a agent.log
