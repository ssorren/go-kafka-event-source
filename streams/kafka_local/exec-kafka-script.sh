BASEDIR=$(dirname "$0")
START_DIR=$(pwd)
if [ -z $KAFKA_INSTALL ]
then
    KAFKA_INSTALL=$1
fi

PROGRAM=$2
COMMAND=$3

cd ${KAFKA_INSTALL}
ABS_KAFKA_INSTALL=$(pwd)
cd ${START_DIR}/${BASEDIR}
echo 1
${ABS_KAFKA_INSTALL}/bin/${PROGRAM}-server-${COMMAND}.sh ./${PROGRAM}.properties > ./${PROGRAM}.log
echo 2