
## How to build the application using alpine container

To build the application using alpine docker container you can use make command.
Current Makefile have a following targets:
- alpine - create a base image for building the application. This image have all necessary dependencies installed inside.
- base - create base container which will be used for further building process
- shell - login into base container console for manual manipulation if it's necessary
- build - create development version of the application with all necessary debug information and placing it into ~/bin/yc binary
- deploy - create optimized production version of the application and placing it into ~/bin/yc_deploy binary  

Usually you should once execute 
```shell
make alpine base 
```

and when execute 
```shell
make build 
```
each time to rebuild the application