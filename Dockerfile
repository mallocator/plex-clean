FROM node:18-alpine

WORKDIR /work

COPY package.json clean.js ./

RUN npm install

# Specify the port on which the webhook is going to be listening
ENV PORT=3333
# Specify how much of the episode needs to have been watched to create the file. Float between 0 - 100 should be okay.
ENV THRESHOLD=97
# Specify a second (independant) trigger if we stopped watching with less than x minutes left
ENV TIMELEFT=120

CMD ["node", "."]

# The folder where output files will be written to
VOLUME /output