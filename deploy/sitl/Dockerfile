# ArduCopter SITL for the Compose `sitl` profile (plan open item 9). Compiling
# ArduPilot takes several minutes, which is why the profile is optional; the
# image that runs holds only the binary and its parameters.
#   docker build -t murmur/sitl deploy/sitl
FROM ubuntu:24.04 AS build
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential ca-certificates g++ gcc git python3 python3-pip python3-venv
RUN python3 -m venv /venv && /venv/bin/pip install --no-cache-dir \
    dronecan==1.0.27 empy==3.3.4 future==1.0.0 pexpect==4.9.0 pymavlink==2.4.49 setuptools
# The commit Murmur's stream-rate parameters and recordings were tested on.
ARG ARDUPILOT_COMMIT=9165d22419406556fc9815d9f4e3e299e13713a8
WORKDIR /ardupilot
RUN git init -q . && git remote add origin https://github.com/ArduPilot/ardupilot.git \
    && git fetch -q --depth 1 origin "$ARDUPILOT_COMMIT" && git checkout -q FETCH_HEAD \
    && git submodule update -q --init --recursive --depth 1
RUN PATH=/venv/bin:$PATH sh -c './waf configure --board sitl && ./waf copter'

FROM ubuntu:24.04
RUN useradd -m -u 10001 murmur
COPY --from=build /ardupilot/build/sitl/bin/arducopter /ardupilot/build/sitl/bin/arducopter
COPY --from=build /ardupilot/Tools/autotest/default_params/copter.parm /ardupilot/Tools/autotest/default_params/copter.parm
COPY run-fleet.sh murmur-streams.parm /sitl/deploy/sitl/
RUN mkdir /sitl/.sitl && chown murmur /sitl/.sitl
USER murmur
ENV ARDUPILOT=/ardupilot
ENTRYPOINT ["/sitl/deploy/sitl/run-fleet.sh"]
CMD ["3", "ingester:14560"]
