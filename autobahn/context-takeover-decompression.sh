#!/bin/bash

if [ "$(uname)" = "Linux" ]; then
    docker run -it --rm  --net=host -v ${PWD}/config:/config -v /root/report:/report crossbario/autobahn-testsuite wstest -m fuzzingclient -s /config/fuzzingclient-context-takeover-decompression.json
else
    echo "not linux"
    docker run -it --rm  --net=host -v ${PWD}/config:/config -v ${PWD}/report:/report crossbario/autobahn-testsuite wstest -m fuzzingclient -s /config/fuzzingclient-context-takeover-decompression.json
fi

#docker run -it --rm  --net=host -v ${PWD}/config:/config -v ${PWD}/report:/report crossbario/autobahn-testsuite wstest -m fuzzingclient -s /config/fuzzingclient.json 