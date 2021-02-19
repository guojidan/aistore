#!/bin/bash
set -e
sudo /home/ubuntu/ais/bin/aisnode -config=/home/ubuntu/ais.json -local_config=/home/ubuntu/ais_local.json -role=target &
if ! ps -C aisnode -o pid= ; then
	echo target started on host $(hostname)
else
	echo failed to start target on host $(hostname)
fi
