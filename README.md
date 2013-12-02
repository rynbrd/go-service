Go Service!
===========
The go-service library is a channel driver wrapper around os/exec. It
provides an event mechanism for monitoring a process and a command channel for
controlling the process. Think supervisord in a Go library. In fact, the states
and state transitions are identical to Supervisor. As such it is best suited
for managing long running foreground processes.

See the example program for general usage.

License
-------
The go-service library is covered under a BSD style license under copyright of
the authors. See the included LICENSE file for the full license contents.

Authors
-------
The following people are authors of the go-service library:
- Ryan Bourgeois <bluedragonx@gmail.com> (maintainer)
