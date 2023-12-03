import fs          from 'fs';
import http        from 'http';
import multer      from 'multer';

const port = process.env.PORT || 3333;
const apiHost = process.env.API_HOST
const apiKey = process.env.API_KEY
const threshold = process.env.THRESHOLD || 95;
const debug = process.env.DEBUG === 'true' || false;

async function request(options) {
  return new Promise((resolve, reject) => {
    const req = http.request(options, (res) => {
      let data = ''
      res.on('data', chunk => data += chunk)
      res.on('end', () => resolve(JSON.parse(data)))
    });
    req.on('error', error => reject(error))
    req.end()
  });
}

async function fetchMetadata(path) {
  if (!path) {
    return;
  }
  
  const key = path.substring(path.indexOf('/library/metadata/') + 18)
  if (!key.length) {
    return;
  }
  
  const result = await request(`http://${apiHost}/api/v2?apikey=${apiKey}&cmd=get_history&rating_key=${key}&order_column=started&order=desc&length=1`)
  return result.response?.data?.data ?? []
}

function end(res, body = 'OK', code=200) {
  res.writeHead(code, {'Content-Type': 'text/plain'})
  res.end(body)
}

const demultiplexer = multer()

// Create an HTTP server
http.createServer((req, res) => {
  demultiplexer.single('thumb')(req, res, async (err) => {
    if (err) {
      console.log('Unable to decode response from Plex server', err)
      return end(res,500,'Error')
    } else {
      const payload = JSON.parse(req.body.payload)
      if (payload.event !== 'media.stop' ) {
        debug && console.log('Ignoring event', payload.event)
        return end(res);
      }
      if (!payload.Metadata) {
        debug && console.log('Invalid request, No metadata found', payload)
        return end(res);
      }
      const key = payload.Metadata?.key
      const dates = await fetchMetadata(key)
      if (!dates.length) {
        console.log('No entries found for', key)
        return end(res);
      } else if (debug) {
        console.log('Found', dates.length, 'entries for', key)
      }
      
      for (const data of dates) {
        if (data.percent_complete > threshold) {
          const filename = `${data.full_title} - S${data.parent_media_index}E${data.media_index}.json`
          console.log(`${data.percent_complete}% above threshold (${threshold}%), writing to file ${filename}: `)
          fs.writeFileSync(`/output/${filename}`, JSON.stringify(data, null, 2))
        } else if (debug) {
          console.log(`${data.percent_complete}% below threshold (${threshold}%), ignoring`)
        }
      }
      return end(res);
    }
  })
}).listen(port, () => console.log(`Server v1.1 running on port ${port}`))

