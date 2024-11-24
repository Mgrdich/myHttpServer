# HTTP Server Implementation from Scratch
* implement `HTTP/1.1` Server using `tcp`
* implement `HTTP/2.0` Server using `tcp`
* making sure it uses multi-threading and connection persistence for each case as defined per protocol
* This example uses `HTTP/2` automatically because the server is `HTTPS`.
* `HTTP/2` requires `TLS` (which is why `ListenAndServeTLS` is used instead of `ListenAndServe`).



## How to build the project
* Create self signed ssl certificates by running `openssl req -new -x509 -days 365 -nodes -out server.crt -keyout server.key`
* run `make build_ci` to build the executable and then `./main-exec`
* or you can `make run`