FROM image.midea.com/library/busybox:1.34.0
RUN mkdir /.runtime
COPY chimera /.runtime/chimera
COPY config.yaml /.runtime/config.yaml
RUN chmod +x /.runtime/chimera
WORKDIR /.runtime/
CMD ./chimera 

