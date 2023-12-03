#!/usr/bin/env bash

docker build --platform linux/amd64 -t plex-cleaner .

docker image tag plex-cleaner mallox/plex-cleaner:1.1

docker push mallox/plex-cleaner:1.1

docker image tag plex-cleaner mallox/plex-cleaner:latest

docker push mallox/plex-cleaner:latest