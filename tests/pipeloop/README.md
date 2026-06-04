This is directly inspired by dpipe(1).

dpipe's documentation describes running two processes with their respective
stdin and stdout descriptors tied together so that one process's output is fed
into the other and vice-versa.

pipeloop attempts to generalize this to cases where there are two _or more_
such processes.  Each process has its stdout connected to the next process's
stdin, and the last has its stdout connected to the stdin of the first process,
forming a complete loop.  When one of them exits, the others are killed.
