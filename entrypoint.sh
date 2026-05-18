#!/bin/sh
GIT_TOKEN="${GH_TOKEN:-$GITHUB_TOKEN}"
if [ -n "$GIT_TOKEN" ]; then
    printf 'machine github.com\nlogin x-access-token\npassword %s\n' "$GIT_TOKEN" > /root/.netrc
    chmod 600 /root/.netrc
fi
exec pr-review-bot "$@"
