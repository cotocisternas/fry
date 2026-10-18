FROM scratch
COPY ryuk /ryuk
EXPOSE 8080
ENTRYPOINT ["/ryuk"]
