FROM alpine
RUN echo that-is-stdout | sed s,that,this,g >&1
RUN echo that-is-stderr | sed s,that,this,g >&2
