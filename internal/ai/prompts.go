package ai

// The prompts are ported from the Python service so a reel processed by Go
// produces the same shape of extraction as one processed before the migration.

const transcriptionPrompt = `Transcribe the spoken audio verbatim in its original language. ` +
	`Return only the transcript text, with no commentary, timestamps, or ` +
	`speaker labels. If there is no intelligible speech, return nothing.`

const imageTextPrompt = `Read every piece of text visible in these images: captions, overlays, signs,
menus, prices, handles and hashtags. Return the text as plain lines, in reading order,
with no commentary. If there is no readable text, return an empty response.`

const extractionPrompt = `You are an AI that analyzes short-form video and image-post content. Given the transcript (and optional caption), extract structured information.

TRANSCRIPT:
{transcript}

CAPTION:
{caption}

Extract the following as a JSON object:
{
    "title": "A concise, descriptive title for this reel (max 10 words)",
    "summary": "A 2-3 sentence summary of what this reel is about",
    "content_domain": "A broad natural language domain like Movies, Fitness, Food, Travel, Personal Finance, Tech, Fashion, Cricket, News, TV & Series",
    "content_format": "A reusable content type like Trailers, Reviews, Scenes, Recipes, Workout Tips, Match Highlights, Product Reviews, News Updates, Tutorials",
    "topical_tags": ["3 to 6 concise topic tags that capture specific themes, subjects, or entities mentioned"],
    "key_facts": ["List of specific facts, tips, or pieces of information mentioned"],
    "locations": [
        {
            "name": "Name of the place (restaurant, cafe, landmark, attraction, etc.)",
            "neighborhood": "Neighborhood or specific local area",
            "city": "City name",
            "state": "State or province",
            "country": "Country name"
        }
    ],
    "people_mentioned": ["Names of people, creators, or brands mentioned"],
    "actionable_items": ["Things the viewer might want to do based on this reel"],
    "events": [
        {
            "name": "What is happening (e.g. concert, movie release, festival, match, registration deadline)",
            "date": "ISO date YYYY-MM-DD if a specific calendar date is stated or clearly implied, else empty string",
            "time": "24-hour HH:MM if a specific time is stated, else empty string"
        }
    ]
}

Rules:
- Only include locations the user could actually visit and would want saved as a map pin: specific named restaurants, cafes, bars, shops, markets, attractions, landmarks, hotels, gyms, parks, or venues. Judge whether each place is genuinely useful to save, not just whether it was mentioned.
- EXCLUDE incidental or contextual mentions: cities, states, or countries named only as context or transit; broad regions; fictional or movie/TV settings; and the creator's home location when it is not the subject. Only keep a city or region on its own when it is clearly the actionable destination of the reel and no more specific venue is given.
- Extract every distinct USEFUL place (if five real venues are recommended, return all five) and never merge multiple places into one item.
- For the places you keep, ALWAYS include the city and country even if not explicitly stated, and correct obvious phonetic spelling mistakes in place, city, or neighborhood names, so the place can be found on Google Maps.
- If no useful, visitable locations are mentioned, return an empty locations array.
- Never use null for location string fields. Use an empty string instead.
- If no people are mentioned, return an empty array.
- Be specific with facts; don't be vague.
- content_domain should be a clean user-facing label, not a sentence.
- content_format should describe the kind of content, not the broad domain.
- topical_tags should be short, concrete, and deduplicated.
- Only include an event when it has a concrete date the user would want a reminder for (a concert, release, festival, match, sale, or deadline). Do NOT invent dates: if no specific date is stated or clearly implied, leave the events array empty. Never guess a year; only set date when the full YYYY-MM-DD is unambiguous.
- Return ONLY the JSON object, no other text.`

const categoryPrompt = `You organize saved reels for a single user.

Your job is to assign each reel into a clean 2-level personal taxonomy:
- category: broad, reusable bucket, 1-3 words
- subcategory: more specific reusable bucket under that category, 1-4 words

CONTENT:
{content}

EXISTING USER CATEGORY TREE:
{existing}

Return JSON only:
{
  "category": "Primary category label",
  "subcategory": "Primary subcategory label",
  "secondary_categories": ["Up to 2 extra related labels if truly useful"]
}

Rules:
- Reuse an existing category and subcategory when they fit well.
- Only create a new category if the current tree does not fit.
- Avoid vague labels like Other, General, Misc, Entertainment, Lifestyle, Viral, Funny.
- Keep labels user-facing and clean.
- Use title case.
- Do not include punctuation-heavy labels.
- For movie or film content, prefer clear buckets like Movies > Trailers, Movies > Reviews, Movies > Scenes, Movies > Fan Edits, Movies > News.
- For TV or series content, prefer TV & Series > Trailers, TV & Series > Reviews, TV & Series > Scenes, TV & Series > News.
- secondary_categories should be concise and optional. Return an empty array when not needed.`
