#!/bin/bash

if [ "$(uname)" = "Linux" ]; then
    docker run -it --rm  --net=host -v ${PWD}/config:/config -v /root/report:/report crossbario/autobahn-testsuite wstest -m fuzzingclient -s /config/fuzzingclient-io.json
else
    echo "not linux"
    docker run -it --rm  --net=host -v ${PWD}/config:/config -v ${PWD}/report:/report crossbario/autobahn-testsuite wstest -m fuzzingclient -s /config/fuzzingclient-io.json
fi

#docker run -it --rm  --net=host -v ${PWD}/config:/config -v ${PWD}/report:/report crossbario/autobahn-testsuite wstest -m fuzzingclient -s /config/fuzzingclient.json 
