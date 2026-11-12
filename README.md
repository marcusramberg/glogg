# Gløgg - Google loggalizer

This is a simple TUI whipped together by Claude to spare me from the pain of using
google's log explorer user interface.

```sh
go install github.com/marcusramberg/glogg@latest
```

Note: to avoid running out of quota you need to set quota project to a project
you are editor or owner in:

```sh
gcloud auth application-default set-quota-project <my-project>
```
