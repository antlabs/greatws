docker run -it --rm  --net=host -v ${PWD}/config:/config -v ${PWD}/report:/report crossbario/autobahn-testsuite wstest -m fuzzingclient -s /config/fuzzingclient.json 
